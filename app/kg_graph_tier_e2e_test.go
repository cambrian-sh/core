package app

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/kernel"
	"github.com/cambrian-sh/core/internal/metabolism"
	"github.com/cambrian-sh/core/internal/metabolism/agentmgr"
)

// T-20261118-kg-extractor-graph-tier-cambrian-kg-graph-tier end to end:
// execution.ingestion.kg_graph_tier, set in a config file and loaded by the
// kernel's own loader, reaches the SHIPPED kg_extractor system agent running
// under the agent sandbox on a kernel-managed Python env, and selects its
// graph-node tier. The path is the production one:
//
//	config.LoadConfig (file layer, validation)
//	→ kernel.AgentEnvExtras (what NewMetabolismStack hands SetEnvExtras)
//	→ AgentManager.CallAgent (boot under the sandbox, dial, gRPC Execute) with
//	  the chunk_triplet_request payload KgExtractorDispatcher sends
//	→ agents/system/kg_extractor_agent reads CAMBRIAN_KG_GRAPH_TIER.
//
// The tier shows in the reply: every graph-tier triplet names its tier in
// "sources" — spacy_patterns for svo (the default), ner_coref for ner.
//
// Needs the kernel-managed env's interpreter, built from a lock with the SDK
// and spaCy (the release lock or an SDK + spaCy slice of it) so the pinned
// en_core_web_sm model asset is installed (ADR-0157 D7.2.5):
//
//	KGTIER_E2E_PY   the interpreter `cambrian-orchestrator python-env status` prints
//
// Linux runs the sandbox as bubblewrap, Windows as the AppContainer.
func TestE2E_KgGraphTierFromConfigReachesSandboxedAgent(t *testing.T) {
	py := os.Getenv("KGTIER_E2E_PY")
	if py == "" {
		t.Skip("set KGTIER_E2E_PY to a kernel-managed env with spaCy to run the kg graph-tier e2e")
	}
	repo, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	agents := filepath.Join(repo, "agents")
	const agentID = "kg_extractor_agent"
	reg := metabolism.NewInMemoryRegistry()
	reg.SetAgent(domain.AgentDefinition{
		ID: agentID, Name: agentID, Runtime: domain.RuntimePython, ExecPath: "agent.py",
		Dir: filepath.Join(agents, "system", agentID), System: true,
	})

	base, err := os.MkdirTemp("", "kgt")
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(base, 0o755) // a dropped uid (Linux root) must reach the work root
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	kernelDir := filepath.Join(base, "kernel")
	if err := os.MkdirAll(filepath.Join(kernelDir, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mgr := agentManagerWithPython(t, agentmgr.NewAgentManager(reg, lis.Addr().String(), nil), py)
	im := mgr.InstanceManager
	im.SetAgentBootTimeout(90 * time.Second)
	im.SetAgentContainment(agentmgr.AgentContainment{Sandbox: agentmgr.NewAgentSandbox(agentmgr.AgentSandboxOptions{
		WorkRoot:         filepath.Join(base, "w"),
		KernelDir:        kernelDir,
		KernelStatePaths: []string{filepath.Join(kernelDir, "configs")},
		RuntimeReadPaths: []string{agents},
	})})
	// A sandboxed agent reaches the kernel only through its unix socket
	// (ADR-0157 D3/S5); the kg_extractor makes no kernel call, so an empty
	// server is enough.
	srv := grpc.NewServer()
	sockets := newAgentSocketServer()
	sockets.start(srv)
	go func() { _ = srv.Serve(lis) }()
	im.SetAgentTransport(agentmgr.AgentTransport{KernelSocket: sockets.Open})
	t.Cleanup(func() {
		_ = im.KillAllAgents(context.Background())
		srv.Stop()
		sockets.stop()
	})

	texts := []string{
		"Alice Johnson founded Acme Robotics in Paris.",
		"Bob Smith works at Globex Corporation in Berlin. Bob Smith met Alice Johnson.",
	}
	for _, tc := range []struct {
		name, tuning, want, notWant string
	}{
		// No key: the default config's "svo".
		{"default_svo", `{"schema_version":2}`, "spacy_patterns", "ner_coref"},
		{"ner", `{"schema_version":2,"execution":{"ingestion":{"kg_graph_tier":"ner"}}}`, "ner_coref", "spacy_patterns"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bundle := t.TempDir()
			writeKgTierFile(t, bundle, "config.json", `{"database":{"host":"localhost","user":"u","password":"p","dbname":"d"}}`)
			writeKgTierFile(t, bundle, "tuning.json", tc.tuning)
			cfg, err := config.LoadConfig(filepath.Join(bundle, "config.json"))
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			extras := kernel.AgentEnvExtras(cfg)
			t.Logf("kg_graph_tier=%q → agent env %v", cfg.Execution.Ingestion.KgGraphTier,
				slices.DeleteFunc(slices.Clone(extras), func(kv string) bool { return !strings.HasPrefix(kv, "CAMBRIAN_KG_") }))
			// A fresh process per arm: the tier is read once, at agent start.
			_ = im.KillAllAgents(context.Background())
			mgr.SetEnvExtras(extras)

			req, _ := json.Marshal(map[string]any{"texts": texts, "ids": []string{"doc-a", "doc-b"}})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			start := time.Now()
			resp, err := mgr.CallAgent(ctx, agentID, &domain.Handoff{
				FromAgent: "orchestrator", ToAgent: agentID,
				Payload: &domain.Payload{Type: "chunk_triplet_request", Data: req},
				Context: map[string]string{"task_id": "kg-tier-" + tc.name},
			})
			if err != nil {
				t.Fatalf("kg_extractor under the sandbox: %v", err)
			}
			if resp == nil || resp.Payload == nil {
				t.Fatal("empty reply")
			}
			var out struct {
				Triplets [][]struct {
					H, R, T string
					Sources []string `json:"sources"`
				} `json:"triplets"`
			}
			if err := json.Unmarshal(resp.Payload.Data, &out); err != nil {
				t.Fatalf("reply %q: %v", resp.Payload.Data, err)
			}
			if len(out.Triplets) != len(texts) {
				t.Fatalf("want %d positional triplet lists, got %d: %s", len(texts), len(out.Triplets), resp.Payload.Data)
			}
			got, wrong := 0, 0
			var sample []string
			for _, chunk := range out.Triplets {
				for _, tr := range chunk {
					if slices.Contains(tr.Sources, tc.want) {
						got++
						if len(sample) < 6 {
							sample = append(sample, "("+tr.H+" | "+tr.R+" | "+tr.T+")")
						}
					}
					if slices.Contains(tr.Sources, tc.notWant) {
						wrong++
					}
				}
			}
			t.Logf("%s: %d triplets from %s in %s: %s", tc.name, got, tc.want,
				time.Since(start).Round(time.Millisecond), strings.Join(sample, " "))
			if got == 0 {
				t.Fatalf("no triplets from the %s tier: %s", tc.want, resp.Payload.Data)
			}
			if wrong != 0 {
				t.Fatalf("%d triplets from %s, the tier the config did not select: %s", wrong, tc.notWant, resp.Payload.Data)
			}
		})
	}
}

func writeKgTierFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
