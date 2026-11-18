package app

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/agentplane"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/kernel"
	"github.com/cambrian-sh/core/internal/memory/vault"
	"github.com/cambrian-sh/core/internal/metabolism"
	"github.com/cambrian-sh/core/internal/metabolism/agentmgr"
	"github.com/cambrian-sh/core/internal/storage"
	"github.com/cambrian-sh/core/internal/substrate/network"
	"github.com/cambrian-sh/core/internal/testing/pgscratch"
)

// ENG-2095 end to end: two REAL Python SDK agents run under the sandbox; the
// kernel dispatches each a step (agentplane.Transport.CallAgent, a handoff
// carrying the step's lease) and each writes a file to its out/ folder during
// the step and stays alive. Then the kernel shuts down gracefully, driven
// in-process (Windows cannot send a console Ctrl-C to a detached kernel):
// AgentManager.Shutdown — what MetabolismStack.Shutdown runs — and, the moment
// it returns, the ledger's pool is closed, as Kernel.Shutdown closes the store
// after the metabolism stack.
//
//   - q77_live's out/ file is in the ledger with the step's run, session and
//     step: it was collected BEFORE the pool closed;
//   - q77_hung's collection hangs: the wait hits
//     execution.agents.shutdown_timeout_ms, Shutdown still returns, naming
//     that instance;
//   - a dispatch after shutdown boots nothing (ErrShuttingDown).
//
// Needs the kernel-managed env's interpreter with the SDK
// (STEP_ARTIFACTS_E2E_PY) and a migrated, disposable Postgres (PG_TEST_DSN).
func TestE2E_GracefulShutdownCollectsLiveAgentsOutputs(t *testing.T) {
	py, dsn := os.Getenv("STEP_ARTIFACTS_E2E_PY"), os.Getenv("PG_TEST_DSN")
	if py == "" || dsn == "" {
		t.Skip("set STEP_ARTIFACTS_E2E_PY and PG_TEST_DSN to run the graceful-shutdown e2e")
	}
	pgscratch.Migrated(t) // migrates PG_TEST_DSN to HEAD itself (ENG-2100)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	// checkPool reads the ledger after the kernel's pool is closed.
	checkPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(checkPool.Close)
	clean := func() {
		_, _ = checkPool.Exec(context.Background(), `DELETE FROM ledger_entries WHERE run_id LIKE 'q77e-%' OR session_id LIKE 'q77e-%' OR writer LIKE 'agent:q77_%'`)
	}
	clean()
	t.Cleanup(clean)
	ledgerPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	ledgerClosed := false
	t.Cleanup(func() {
		if !ledgerClosed {
			ledgerPool.Close()
		}
	})

	base, err := os.MkdirTemp("", "q77s")
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(base, 0o755)
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	code := filepath.Join(base, "code")
	if err := os.MkdirAll(code, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(code, "q77_step.py"), []byte(shutdownOutputsProbePy), 0o644); err != nil {
		t.Fatal(err)
	}
	av := vault.NewArtifactVault(filepath.Join(base, "vault"))
	store, err := storage.NewBBoltAdapterNoScan(filepath.Join(base, "agents.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	reg := metabolism.NewInMemoryRegistry()
	agents := []string{"q77_live", "q77_hung"}
	for _, id := range append([]string{"q77_fresh"}, agents...) {
		reg.SetAgent(domain.AgentDefinition{ID: id, Runtime: domain.RuntimePython, ExecPath: "q77_step.py", Dir: code})
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mgr := agentManagerWithPython(t, agentmgr.NewAgentManager(reg, lis.Addr().String(), nil), py)
	im := mgr.InstanceManager
	im.SetAgentBootTimeout(60 * time.Second)
	const shutdownTimeout = 15 * time.Second
	im.SetShutdownTimeout(shutdownTimeout)
	kernelDir := filepath.Join(base, "kernel")
	if err := os.MkdirAll(filepath.Join(kernelDir, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	im.SetAgentContainment(agentmgr.AgentContainment{Sandbox: agentmgr.NewAgentSandbox(agentmgr.AgentSandboxOptions{
		WorkRoot: filepath.Join(base, "w"), KernelDir: kernelDir, KernelStatePaths: []string{filepath.Join(kernelDir, "configs")},
	})})
	sockets := newAgentSocketServer()
	im.SetAgentTransport(agentmgr.AgentTransport{KernelSocket: sockets.Open})

	exec := config.ExecutionConfig{
		Session: config.SessionConfig{SessionTokenTTLMultiplier: 5},
		LLM:     config.LLMConfig{LLMGatewayMaxConcurrency: 4},
		Ledger:  config.LedgerConfig{Enabled: true, Artifacts: config.LedgerArtifactsConfig{MaxFileBytes: 4096}},
	}
	gw := network.NewLLMGateway(exec, llm.NewSpendMeter(0, nil, nil))
	az := domain.AllowAllAuthorizer{}
	srvImpl := &network.Server{LLMGateway: gw, Manager: mgr, Authz: az, ExecCfg: exec}
	if sw := wireLedger(ctx, exec.Ledger, exec.Content, ledgerPool, av, kernel.NewAgentRepoDecorator(store), az, false, srvImpl,
		network.NewTurnDelegations(), nil); sw == nil {
		t.Fatal("the run ledger did not come up (is PG_TEST_DSN migrated?)")
	}
	// q77_hung's collection never finishes on its own (a stuck reclaim or
	// ledger write); every other instance goes to the real collector.
	release := make(chan struct{})
	defer close(release)
	im.SetOutputCollector(&hangingCollector{Server: srvImpl, hungAgent: "q77_hung", release: release})

	verifier, err := authz.NewAgentCredentialVerifier(nil, im.Tokens(), kernelSpawnedAgent(reg), false)
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(authz.UnarySurfaceInterceptor(nil), authz.UnaryAgentAuthInterceptor(verifier, nil)),
		grpc.ChainStreamInterceptor(authz.StreamSurfaceInterceptor(nil), authz.StreamAgentAuthInterceptor(verifier, nil)),
	)
	pb.RegisterOrchestratorServer(srv, srvImpl)
	sockets.start(srv)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		_ = im.KillAllAgents(context.Background())
		srv.Stop()
		sockets.stop()
	})

	transport := agentplane.New(mgr)
	instances := map[string]string{}
	for _, id := range agents {
		run, sess := "q77e-run-"+id, "q77e-sess-"+id
		lease, err := gw.Acquire(ctx, domain.StepAllocation{}, 4096, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		gw.BindLease(lease, domain.LeaseBinding{AgentID: id, RunID: domain.RunID(run), StepIndex: 0, SessionID: domain.SessionID(sess)})
		resp, err := transport.CallAgent(ctx, id, &domain.Handoff{
			Payload: &domain.Payload{Type: "text", Data: []byte("write the report")},
			Context: map[string]string{"_session_token_id": string(lease)},
		}, "")
		if err != nil {
			t.Fatalf("%s: dispatch the step: %v", id, err)
		}
		if got := strings.TrimSpace(string(resp.Payload.Data)); got != "ok" {
			t.Fatalf("%s: the step answered %q", id, got)
		}
		inst, ok := im.GetInstance(id)
		if !ok {
			t.Fatalf("%s: no live instance after the step", id)
		}
		instances[id] = inst.ID
	}
	if n := len(rawEntries(t, ctx, checkPool, `SELECT id FROM ledger_entries WHERE tool_name = 'agent_out' AND writer LIKE 'agent:q77_%'`)); n != 0 {
		t.Fatalf("%d out/ entries before shutdown; the instances should still be alive", n)
	}

	// The graceful shutdown, then the ledger closes at once (Kernel.Shutdown order).
	start := time.Now()
	shutErr := mgr.Shutdown(context.Background())
	elapsed := time.Since(start)
	ledgerPool.Close()
	ledgerClosed = true

	if shutErr == nil || !strings.Contains(shutErr.Error(), instances["q77_hung"]) {
		t.Fatalf("Shutdown error = %v, want it to name the hung instance %s", shutErr, instances["q77_hung"])
	}
	if strings.Contains(shutErr.Error(), instances["q77_live"]) {
		t.Fatalf("Shutdown reported the collected instance as dropped: %v", shutErr)
	}
	if elapsed < shutdownTimeout-time.Second || elapsed > shutdownTimeout+20*time.Second {
		t.Fatalf("Shutdown took %s; want about the %s bound", elapsed, shutdownTimeout)
	}

	outs := rawEntries(t, ctx, checkPool, `SELECT id FROM ledger_entries WHERE tool_name = 'agent_out' AND writer = 'agent:q77_live'`)
	if len(outs) != 1 {
		t.Fatalf("q77_live: out/ entries in the ledger after shutdown = %d, want 1", len(outs))
	}
	o := outs[0]
	if o.instance != instances["q77_live"] || o.run != "q77e-run-q77_live" || o.session != "q77e-sess-q77_live" || o.step != 0 ||
		!strings.Contains(o.args, "out/report.csv") {
		t.Fatalf("q77_live: out/ provenance %+v (want run q77e-run-q77_live session q77e-sess-q77_live step 0 instance %s)", o, instances["q77_live"])
	}
	if n := len(rawEntries(t, ctx, checkPool, `SELECT id FROM ledger_entries WHERE tool_name = 'agent_out' AND writer = 'agent:q77_hung'`)); n != 0 {
		t.Fatalf("q77_hung: %d out/ entries, want none (its collection hung)", n)
	}

	// No new work once shutdown began: an agent with no instance is not booted.
	if _, err := transport.CallAgent(ctx, "q77_fresh", &domain.Handoff{
		Payload: &domain.Payload{Type: "text", Data: []byte("again")},
	}, ""); !errors.Is(err, agentmgr.ErrShuttingDown) {
		t.Fatalf("a dispatch after shutdown: err = %v, want ErrShuttingDown", err)
	}
}

// hangingCollector forwards every instance's out/ to the server, except the
// hung agent's, which blocks until release (ignoring ctx, as a stuck call
// would). It forwards the dispatch observations so leases still bind.
type hangingCollector struct {
	*network.Server
	hungAgent string
	release   <-chan struct{}
}

func (c *hangingCollector) CollectAgentOutputs(ctx context.Context, out domain.AgentOutputs) (int, error) {
	if out.AgentID == c.hungAgent {
		<-c.release
		return 0, errors.New("released after the test")
	}
	return c.Server.CollectAgentOutputs(ctx, out)
}

// shutdownOutputsProbePy: an SDK CognitiveAgent whose step writes
// out/report.csv and answers "ok"; the instance stays alive after it.
const shutdownOutputsProbePy = `
import os
from cambrian_agent_sdk import CognitiveAgent

AGENT = os.environ.get("CAMBRIAN_AGENT_ID", "")

class Step(CognitiveAgent):
    def run(self, task):
        try:
            out = os.environ["CAMBRIAN_AGENT_OUT"]
            os.makedirs(out, exist_ok=True)
            with open(os.path.join(out, "report.csv"), "w", newline="") as f:
                f.write("a,b\n1,2\n")
            return "ok"
        except Exception as e:
            return "ERROR " + repr(e)

Step(agent_id=AGENT).serve()
`

// The kernel's shutdown budget leaves the whole agent wait on top of the 10 s
// the rest of the sequence gets (ENG-2095).
func TestKernelShutdownBudget_AddsTheAgentWait(t *testing.T) {
	if got := kernelShutdownBudget(nil); got != 10*time.Second {
		t.Fatalf("budget without a metabolism stack = %s, want 10s", got)
	}
	m := agentmgr.NewAgentManager(metabolism.NewInMemoryRegistry(), "127.0.0.1:1", nil)
	m.SetShutdownTimeout(25 * time.Second)
	k := &Kernel{Metabolism: &kernel.MetabolismStack{Manager: m}}
	if got := kernelShutdownBudget(k); got != 35*time.Second {
		t.Fatalf("budget = %s, want 35s", got)
	}
}
