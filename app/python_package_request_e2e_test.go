package app

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/metabolism"
	"github.com/cambrian-sh/core/internal/metabolism/agentmgr"
	"github.com/cambrian-sh/core/internal/pypackages"
	"github.com/cambrian-sh/core/internal/testing/pgscratch"
)

// ADR-0157 D7.9-D7.10 (P4) live, on the managed env with the real pinned uv:
// a running Python agent that declares nothing asks for a package through the
// request_python_package tool; an Operator approves inside the window ⇒
// installed into the agent's own layer; the layer change recycles the agent's
// idle instance (AgentManager.MarkAgentStale), and the next instance imports
// the package from its layer.
//
// Same environment as TestE2E_PythonPackageLayerThroughOperatorApproval
// (PYPKG_E2E_KERNEL, PG_TEST_DSN; optional PYPKG_E2E_PACKAGE, PYPKG_E2E_MODULE,
// PYPKG_E2E_WHEELHOUSE); Windows AppContainer or Linux bubblewrap.
func TestE2E_RuntimePackageRequestReachesTheRecycledAgent(t *testing.T) {
	kernelDir, dsn := os.Getenv("PYPKG_E2E_KERNEL"), os.Getenv("PG_TEST_DSN")
	if kernelDir == "" || dsn == "" {
		t.Skip("set PYPKG_E2E_KERNEL and PG_TEST_DSN to run the runtime package request e2e")
	}
	pgscratch.Migrated(t)
	pkgName, pkgVersion, module := "python-dateutil", "2.9.0.post0", "dateutil"
	if v := os.Getenv("PYPKG_E2E_PACKAGE"); v != "" {
		pkgName, pkgVersion, _ = strings.Cut(v, "==")
	}
	if v := os.Getenv("PYPKG_E2E_MODULE"); v != "" {
		module = v
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	const agent = "pypkgreq-analyst"
	cleanRows := func() {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()
		_, _ = tx.Exec(ctx, `SET LOCAL session_replication_role = replica`)
		_, _ = tx.Exec(ctx, `DELETE FROM python_package_events WHERE target = $1`, agent)
		_, _ = tx.Exec(ctx, `DELETE FROM python_package_approvals WHERE target = $1`, agent)
		_, _ = tx.Exec(ctx, `DELETE FROM approvals WHERE agent_id = $1`, agent)
		_ = tx.Commit(ctx)
	}
	cleanRows()
	t.Cleanup(cleanRows)

	cfg := config.DefaultConfig()
	cfg.Execution.Agents.Python.BuildOnBoot = false
	if wh := os.Getenv("PYPKG_E2E_WHEELHOUSE"); wh != "" {
		cfg.Execution.Agents.Python.Wheelhouse = []string{wh}
		cfg.Execution.Agents.Python.Offline = true
	}
	envRoot := cfg.Execution.Agents.Python.ResolvedRoot(absOr(kernelDir))
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(envRoot, "layers", agent)) })

	rec := &recordingHealth{}
	srv := grpc.NewServer()
	healthpb.RegisterHealthServer(srv, rec)
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sockets := newAgentSocketServer()
	sockets.start(srv)
	go func() { _ = srv.Serve(tcp) }()
	defer func() { srv.Stop(); sockets.stop() }()

	work, err := os.MkdirTemp("", "p4e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(work) })
	code, hidden := filepath.Join(work, "code"), filepath.Join(work, "kernel")
	for _, d := range []string{code, filepath.Join(hidden, "configs")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(code, "probe.py"), []byte(packageProbePy), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := metabolism.NewInMemoryRegistry()
	setDef := func(run string) *domain.AgentDefinition {
		d := domain.AgentDefinition{ID: agent, Name: agent, Runtime: domain.RuntimePython, ExecPath: "probe.py", Dir: code,
			TrustTier: "first-party", LaunchArgs: []string{"--probe-id", agent + "-" + run, "--module", module}}
		reg.SetAgent(d)
		return &d
	}
	reg.SetManifest(agent, &domain.AgentManifest{ToolAllowlist: []string{pypackages.ToolName}})

	env := startPythonEnv(ctx, cfg, kernelDir)
	if _, err := env.Interpreter(); err != nil {
		t.Fatalf("the managed base env under %s is not usable: %v", kernelDir, err)
	}
	hub, err := buildApprovalHub(ctx, cfg, pool)
	if err != nil {
		t.Fatal(err)
	}
	m := agentmgr.NewAgentManager(reg, tcp.Addr().String(), nil)
	svc := wirePythonPackages(ctx, cfg.Execution.Agents.Python, pool, env, hub, reg.GetManifest, m.MarkAgentStale)
	if svc == nil {
		t.Fatal("python package service not wired (is PG_TEST_DSN migrated through 0057?)")
	}
	defer svc.Stop()
	m.SetPythonEnv(env)
	m.SetPythonLayers(svc)
	m.SetAgentContainment(agentmgr.AgentContainment{Sandbox: agentmgr.NewAgentSandbox(agentmgr.AgentSandboxOptions{
		WorkRoot: filepath.Join(work, "w"), KernelDir: hidden,
		KernelStatePaths: []string{filepath.Join(hidden, "configs"), envRoot},
		ThirdPartyRefuse: true, FirstPartyRefuse: true,
	})})
	m.SetAgentTransport(agentmgr.AgentTransport{KernelSocket: sockets.Open})
	m.SetAgentBootTimeout(90 * time.Second)
	defer m.EvictAgent(agent)

	bctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	first, err := m.GetOrBootInstance(bctx, setDef("1"), "")
	if err != nil {
		t.Fatalf("an agent that declares nothing boots: %v", err)
	}
	if got := rec.waitFor(bctx, "probe="+agent+"-1;"); !strings.Contains(got, "import=failed(") {
		t.Fatalf("before the request the agent cannot import %s: %s", module, got)
	}

	tool, err := pypackages.NewTool(svc, 4*time.Minute, pypackages.ToolDeps{
		LeaseAgent: func(domain.LeaseID) (string, bool) { return agent, true },
		IsChild:    func(domain.LeaseID) bool { return false },
		Agent: func(ctx context.Context, id string) (*domain.AgentDefinition, *domain.AgentManifest, error) {
			d, err := reg.GetAgentByName(ctx, id)
			if err != nil {
				return nil, nil, err
			}
			man, err := reg.GetManifest(ctx, id)
			return d, man, err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { // the Operator answers inside the window
		for bctx.Err() == nil {
			items, _ := hub.List(ctx, domain.ApprovalQuery{States: []domain.ApprovalState{domain.ApprovalPending}})
			for _, it := range items {
				if it.AgentID == agent && it.Kind == domain.ApprovalKindPythonPackage {
					_, _ = hub.Answer(ctx, it.ID, domain.ApprovalAnswer{Approved: true, By: domain.OperatorApprover("op-e2e")})
					return
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()
	setDef("2") // the next instance reports under a new probe id
	out, err := tool.Call(bctx, domain.KernelToolCall{AgentID: agent, SessionTokenID: "lease-1",
		ArgsJSON: []byte(`{"name":"` + pkgName + `","version":"` + pkgVersion + `","rationale":"e2e"}`)})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("request: %s", out)
	if !strings.Contains(string(out), `"status":"installed"`) {
		t.Fatalf("approved inside the window ⇒ installed: %s", out)
	}
	if _, ok := m.GetInstanceByID(first.ID); ok {
		t.Fatal("the idle instance on the old layer must be recycled")
	}
	next, err := m.GetOrBootInstance(bctx, setDef("2"), "")
	if err != nil {
		t.Fatalf("the recycled agent boots on its new layer: %v", err)
	}
	if next.ID == first.ID {
		t.Fatal("a new instance must serve the agent")
	}
	got := rec.waitFor(bctx, "probe="+agent+"-2;")
	t.Logf("recycled agent: %s", got)
	imp, _, _ := strings.Cut(strings.TrimPrefix(got, "probe="+agent+"-2;import="), ";")
	if !strings.HasPrefix(imp, "ok@") || !strings.HasPrefix(filepath.Clean(strings.TrimPrefix(imp, "ok@")), filepath.Join(filepath.Clean(envRoot), "layers")) {
		t.Fatalf("the recycled agent must import %s from its own layer: %s", module, got)
	}
}
