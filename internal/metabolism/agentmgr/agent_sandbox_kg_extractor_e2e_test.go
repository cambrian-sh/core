package agentmgr

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/sandbox"
)

// ADR-0157 D7.2.5 e2e (ENG-2072): the shipped kg_extractor system agent boots
// on a kernel-managed base generation that carries the pinned en_core_web_sm
// model asset, under the agent sandbox, and extracts triplets with its spaCy
// tier — for the default graph tier ("svo", SpaCyPatternExtractor) and for
// "ner" (NEREntityExtractor), the two tiers that load the model. The tier is
// set through the kernel's config-derived agent environment
// (InstanceManager.SetEnvExtras — CAMBRIAN_* names never pass the env
// allowlist). This package cannot import the kernel, so the test sets the
// extra directly; app TestE2E_KgGraphTierFromConfigReachesSandboxedAgent
// drives it from execution.ingestion.kg_graph_tier through
// kernel.AgentEnvExtras (T-20261118-kg-extractor-graph-tier-cambrian-kg-graph-tier).
//
// The call goes through AgentManager.CallAgent (boot, dial, gRPC Execute) with
// the payload KgExtractorDispatcher sends (internal/substrate/network; that
// package imports this one, so the test rebuilds its request here).
//
// It runs only when the env and a kernel directory are named, like
// TestE2E_ShippedAgentsServeUnderConfinement:
//
//	AGENTMGR_E2E_PY      the managed env's interpreter (`python-env status`),
//	                     built from a lock that contains spacy (the release
//	                     lock, or an SDK + spaCy slice of it) and the
//	                     embedded model-asset table
//	AGENTMGR_E2E_KERNEL  a kernel directory holding agents/, prompts/, skills/
//	                     and configs/
func TestE2E_KgExtractorExtractsOnManagedEnvUnderSandbox(t *testing.T) {
	py, kernel := os.Getenv("AGENTMGR_E2E_PY"), os.Getenv("AGENTMGR_E2E_KERNEL")
	if py == "" || kernel == "" {
		t.Skip("set AGENTMGR_E2E_PY and AGENTMGR_E2E_KERNEL to run kg_extractor on the managed env under the sandbox")
	}
	root := e2eRoot(t)
	sb := NewAgentSandbox(AgentSandboxOptions{
		WorkRoot:         filepath.Join(root, "w"),
		KernelDir:        kernel,
		KernelStatePaths: []string{filepath.Join(kernel, "configs")},
		RuntimeReadPaths: []string{filepath.Join(kernel, "agents"), filepath.Join(kernel, "prompts"), filepath.Join(kernel, "skills")},
	})
	enforced := planAgent(sb).Enforced
	t.Logf("sandbox controls enforced on this host: %v", enforced)
	if !slices.Contains(enforced, sandbox.ControlHideState) {
		t.Log("note: no read confinement on this host; the agent still runs through the sandbox")
	}

	const agentID = "kg_extractor_agent"
	m := newTestManager()
	m.Registry.(*testRegistry).agents[agentID] = domain.AgentDefinition{
		ID: agentID, Name: agentID, Runtime: domain.RuntimePython, ExecPath: "agent.py",
		Dir: filepath.Join(kernel, "agents", "system", agentID), System: true,
	}
	im := NewInstanceManager("127.0.0.1:1")
	im.SetPythonEnv(fixedPythonEnv{pi: managedInterpreterForTest(t, py)})
	im.SetAgentContainment(AgentContainment{Sandbox: sb})
	im.SetAgentTransport(AgentTransport{KernelSocket: listenKernelSocketForTest})
	im.SetAgentBootTimeout(90 * time.Second)
	m.InstanceManager = im

	texts := []string{
		"Alice Johnson founded Acme Robotics in Paris.",
		"Bob Smith works at Globex Corporation in Berlin. Bob Smith met Alice Johnson.",
	}
	for _, tc := range []struct{ tier, source string }{
		{"svo", "spacy_patterns"},
		{"ner", "ner_coref"},
	} {
		t.Run(tc.tier, func(t *testing.T) {
			im.SetEnvExtras([]string{"CAMBRIAN_KG_GRAPH_TIER=" + tc.tier})
			req, _ := json.Marshal(map[string]any{"texts": texts, "ids": []string{"doc-a", "doc-b"}})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			start := time.Now()
			resp, err := m.CallAgent(ctx, agentID, &domain.Handoff{
				FromAgent: "orchestrator", ToAgent: agentID,
				Payload: &domain.Payload{Type: "chunk_triplet_request", Data: req},
				Context: map[string]string{"task_id": "kg-extract"},
			})
			if err != nil {
				t.Fatalf("kg_extractor (%s) under the sandbox: %v", tc.tier, err)
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
			spacy := 0
			var sample []string
			for _, chunk := range out.Triplets {
				for _, tr := range chunk {
					if slices.Contains(tr.Sources, tc.source) {
						spacy++
						if len(sample) < 6 {
							sample = append(sample, "("+tr.H+" | "+tr.R+" | "+tr.T+")")
						}
					}
				}
			}
			t.Logf("tier %s: %d triplets from %s in %s: %s", tc.tier, spacy, tc.source,
				time.Since(start).Round(time.Millisecond), strings.Join(sample, " "))
			if spacy == 0 {
				// The agent swallows a tier's exception and returns the other
				// tiers' triplets, so "no spaCy triplets" is how a missing
				// model shows up here.
				t.Fatalf("no triplets from the spaCy tier %s (model not loadable?): %s", tc.source, resp.Payload.Data)
			}
		})
	}
}
