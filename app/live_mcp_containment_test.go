//go:build livemcp

package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/mcp"
	"github.com/cambrian-sh/core/internal/integrations"
	"github.com/cambrian-sh/core/internal/sandbox/egressproxy"
	"github.com/cambrian-sh/core/internal/stagedinputs"
)

// LIVE check (T-20261106-live-verification-catalog-stdio-servers-under, ENG-720):
// installs real catalog stdio servers through the real integration manager —
// the kernel's install path: catalog entry → store entry → Connector.AddServer
// → sandbox.Start under the trust tier the default sandbox policy resolves —
// and reports, per server, whether it connected, how many tools it advertised
// and the effective containment. It downloads packages (npx/uvx) and starts
// processes, so it never runs in CI: build tag `livemcp` AND
// CAMBRIAN_LIVE_MCP=1. No LLM, no database.
//
//	CAMBRIAN_LIVE_MCP=1 go test -tags livemcp -run TestLive_CatalogStdioServersUnderContainment -v ./app/
//
// CAMBRIAN_LIVE_MCP_IDS (comma-separated) overrides the server list.
func TestLive_CatalogStdioServersUnderContainment(t *testing.T) {
	if os.Getenv("CAMBRIAN_LIVE_MCP") != "1" {
		t.Skip("live check: set CAMBRIAN_LIVE_MCP=1")
	}
	// ${CAMBRIAN_KERNEL_DIR} (code-sandbox) resolves from the kernel bundle root.
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	ids := []string{"sequential-thinking", "calculator", "time", "terminal", "code-sandbox"}
	if v := os.Getenv("CAMBRIAN_LIVE_MCP_IDS"); v != "" {
		ids = filepath.SplitList(v)
	}

	r := newIntegrationRig(t, newAuthServer(t).url)
	cat, err := integrations.Load("")
	if err != nil {
		t.Fatal(err)
	}
	// The kernel's default sandbox policy over the built-in catalog
	// (mcp.sandbox unset): what a fresh install gets. CAMBRIAN_LIVE_MCP_RUNNER
	// (e.g. docker) runs every tier on that runner instead.
	sb := config.MCPSandboxConfig{}
	if runner := os.Getenv("CAMBRIAN_LIVE_MCP_RUNNER"); runner != "" {
		sb.Tiers = map[string]config.MCPSandboxTierConfig{}
		for _, tier := range []string{"first-party", "vetted", "third-party"} {
			sb.Tiers[tier] = config.MCPSandboxTierConfig{Runner: runner}
		}
	}
	// ENG-688: the kernel's egress proxy, as the composition root builds it,
	// so a community server's declared hosts are enforced where the runner can.
	egress := egressproxy.New(egressproxy.Options{})
	if err := egress.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer egress.Stop()
	r.m.runtime.sandbox = sandboxPolicyFromConfig(sb, cat, egress)

	ctx := context.Background()
	for _, id := range ids {
		start := time.Now()
		out, _, err := r.m.Install(ctx, &pb.InstallIntegrationOpRequest{CatalogId: id, Enabled: true})
		if err != nil || out.Effect == "rejected" {
			t.Logf("LIVE %-20s install refused: %+v %v", id, out, err)
			continue
		}
		var h mcp.ServerHealth
		deadline := time.Now().Add(4 * time.Minute)
		for time.Now().Before(deadline) {
			h, _ = r.m.connector.Health(id)
			if h.State == mcp.HealthReady || h.State == mcp.HealthError {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		op := r.find(id)
		summary := op.GetContainment().GetSummary()
		t.Logf("LIVE %-20s state=%s tools=%d after=%s\n      containment: %s\n      last_error: %s",
			id, h.State, h.ToolCount, time.Since(start).Round(time.Second), summary, h.LastError)
		r.m.connector.StopServer(ctx, id, nil)
	}
}

// LIVE check (T-20261110-staged-sandbox-inputs-and-the-offline-sandbox, ENG-1000):
// the code sandbox launched from the catalog (uv --directory ${CAMBRIAN_KERNEL_DIR}),
// called through the real ToolExecutor with a staged path input: the kernel reads
// the file through the filesystem MCP server as the caller, stages it, sends the
// folder in the request _meta over stdio, and the containerised Python reads it
// at /inputs. No LLM, no database (path input; ledger ids need the run ledger).
func TestLive_StagedInputsReachTheCodeSandbox(t *testing.T) {
	if os.Getenv("CAMBRIAN_LIVE_MCP") != "1" {
		t.Skip("live check: set CAMBRIAN_LIVE_MCP=1")
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	files := t.TempDir()
	csv := filepath.Join(files, "expenses.csv")
	if err := os.WriteFile(csv, []byte("item,amount\ntaxi,12.5\nlunch,30\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := newIntegrationRig(t, newAuthServer(t).url)
	cat, err := integrations.Load("")
	if err != nil {
		t.Fatal(err)
	}
	r.m.runtime.sandbox = sandboxPolicyFromConfig(config.MCPSandboxConfig{}, cat, nil)
	ctx := context.Background()
	for _, in := range []*pb.InstallIntegrationOpRequest{
		{CatalogId: "filesystem", Enabled: true, Settings: map[string]string{"FILESYSTEM_ROOT": filepath.ToSlash(files)}},
		{CatalogId: "code-sandbox", Enabled: true},
	} {
		if out, _, err := r.m.Install(ctx, in); err != nil || out.Effect == "rejected" {
			t.Fatalf("install %s: %+v %v", in.GetCatalogId(), out, err)
		}
	}
	for _, id := range []string{"filesystem", "code-sandbox"} {
		deadline := time.Now().Add(4 * time.Minute)
		for time.Now().Before(deadline) {
			if h, _ := r.m.connector.Health(id); h.State == mcp.HealthReady || h.State == mcp.HealthError {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		h, _ := r.m.connector.Health(id)
		t.Logf("LIVE %s state=%s tools=%d err=%s containment=%s", id, h.State, h.ToolCount, h.LastError,
			r.find(id).GetContainment().GetSummary())
		if h.State != mcp.HealthReady {
			t.Fatalf("%s did not come up", id)
		}
	}

	te, err := domain.NewToolExecutor(domain.ToolExecutorControls{
		Registry: r.reg, Grants: domain.NewInMemoryGrantsStore(), Authz: domain.AllowAllAuthorizer{},
		Approval: domain.AlwaysApproveController{}, Unrestricted: true,
	}, domain.ToolExecutor{
		MCPHandler:      &mcp.Handler{Sessions: r.m.connector, CallTimeout: 3 * time.Minute},
		Stager:          stagedinputs.New(stagedinputs.Config{Root: t.TempDir()}, nil),
		InlineThreshold: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{
		"code":   "import pandas as pd\nimport os\nprint(sorted(os.listdir('/inputs')))\ndf = pd.read_csv(list(INPUTS.values())[0])\nprint('TOTAL', df['amount'].sum())",
		"inputs": []string{filepath.ToSlash(csv)},
	})
	resp := te.Execute(ctx, domain.ToolCallRequest{AgentID: "live-check", ToolName: "mcp:code-sandbox/execute_code", ArgsJSON: args})
	t.Logf("LIVE execute_code denied=%v reason=%q error=%q\n      result=%s", resp.Denied, resp.DenyReason, resp.Error, string(resp.ResultJSON))
	if resp.Denied || resp.Error != "" || !strings.Contains(string(resp.ResultJSON), "TOTAL 42.5") {
		t.Fatal("the staged input did not reach the sandboxed Python")
	}
}
