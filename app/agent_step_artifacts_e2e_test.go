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
)

// T-20261118-agent-artifacts-from-planner-lane-steps-carry end to end: the
// kernel DISPATCHES a step (agentplane.Transport.CallAgent, the transport every
// lane uses, a handoff carrying the step's lease) to a REAL Python SDK agent
// under the sandbox; inside run() the agent saves an artifact and writes a file
// to its out/ folder, then the instance exits and the kernel collects out/
// (Postgres ledger, app.wireLedger, the collector wired into the
// InstanceManager). Two agents:
//
//   - q72_sdk saves with the SDK's artifacts.save, passing no lease by hand
//     (the SDK presents the task's lease);
//   - q72_old uploads with a bare stub and NO lease metadata, as SDK 0.1.6 does —
//     only the kernel's dispatch binding can place it.
//
// Asserted for both: the upload row and the out/ row carry the step's run,
// session and step, the instance, and DerivedFrom the step's tool result.
//
// Needs the kernel-managed env's interpreter with this change's SDK
// (STEP_ARTIFACTS_E2E_PY) and a migrated, disposable Postgres (PG_TEST_DSN).
func TestE2E_DispatchedStepArtifactsCarryTheStepsProvenance(t *testing.T) {
	py, dsn := os.Getenv("STEP_ARTIFACTS_E2E_PY"), os.Getenv("PG_TEST_DSN")
	if py == "" || dsn == "" {
		t.Skip("set STEP_ARTIFACTS_E2E_PY and PG_TEST_DSN to run the step-artifacts e2e")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	clean := func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM ledger_entries WHERE run_id LIKE 'q72e-%' OR session_id LIKE 'q72e-%' OR writer LIKE 'agent:q72_%'`)
	}
	clean()
	t.Cleanup(clean)

	base, err := os.MkdirTemp("", "q72s")
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(base, 0o755)
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	code := filepath.Join(base, "code")
	if err := os.MkdirAll(code, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(code, "q72_step.py"), []byte(stepArtifactsProbePy), 0o644); err != nil {
		t.Fatal(err)
	}
	av := vault.NewArtifactVault(filepath.Join(base, "vault"))
	store, err := storage.NewBBoltAdapterNoScan(filepath.Join(base, "agents.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	reg := metabolism.NewInMemoryRegistry()
	agents := []string{"q72_sdk", "q72_old"}
	for _, id := range agents {
		reg.SetAgent(domain.AgentDefinition{ID: id, Runtime: domain.RuntimePython, ExecPath: "q72_step.py", Dir: code})
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mgr := agentManagerWithPython(t, agentmgr.NewAgentManager(reg, lis.Addr().String(), nil), py)
	im := mgr.InstanceManager
	im.SetAgentBootTimeout(60 * time.Second)
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
	if sw := wireLedger(ctx, exec.Ledger, exec.Content, pool, av, kernel.NewAgentRepoDecorator(store), az, false, srvImpl,
		network.NewTurnDelegations(), nil); sw == nil {
		t.Fatal("the run ledger did not come up (is PG_TEST_DSN migrated?)")
	}
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
	for _, id := range agents {
		run, sess := "q72e-run-"+id, "q72e-sess-"+id
		// The step: a lease bound to the agent (as the dispatcher binds it before
		// CallAgent) and one tool result already recorded in it.
		lease, err := gw.Acquire(ctx, domain.StepAllocation{}, 4096, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		gw.BindLease(lease, domain.LeaseBinding{AgentID: id, RunID: domain.RunID(run), StepIndex: 0, SessionID: domain.SessionID(sess)})
		toolRes, err := srvImpl.Ledger.Writer.Record(ctx, domain.LedgerRecord{Kind: domain.LedgerToolResult,
			Provenance: domain.LedgerProvenance{RunID: run, StepIndex: 0, SessionID: sess},
			ToolName:   "crm_get", Writer: domain.AgentPrincipal(id), ContentType: "application/json", Body: []byte(`{"c":1}`)})
		if err != nil {
			t.Fatal(err)
		}
		resp, err := transport.CallAgent(ctx, id, &domain.Handoff{
			Payload: &domain.Payload{Type: "text", Data: []byte("write the report")},
			Context: map[string]string{"_session_token_id": string(lease)},
		}, "")
		if err != nil {
			t.Fatalf("%s: dispatch the step: %v", id, err)
		}
		upID := strings.TrimSpace(string(resp.Payload.Data))
		if !domain.IsLedgerID(upID) {
			t.Fatalf("%s: the agent's upload answered %q", id, upID)
		}
		inst, ok := im.GetInstance(id)
		if !ok {
			t.Fatalf("%s: no live instance after the step", id)
		}
		up := rawEntry(t, ctx, pool, upID)
		if up.writer != "agent:"+id || up.instance != inst.ID || up.run != run || up.session != sess || up.step != 0 ||
			!strings.Contains(up.derived, toolRes.ID) {
			t.Fatalf("%s: upload provenance %+v (want run %s session %s step 0 instance %s derived from %s)", id, up, run, sess, inst.ID, toolRes.ID)
		}

		// The instance exits; its out/ is collected into the step.
		im.EvictInstance(inst.ID)
		var outs []rawRow
		deadline := time.Now().Add(30 * time.Second)
		for {
			outs = rawEntries(t, ctx, pool, `SELECT id FROM ledger_entries WHERE tool_name = 'agent_out' AND writer = 'agent:`+id+`'`)
			if len(outs) >= 1 || time.Now().After(deadline) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if len(outs) != 1 {
			t.Fatalf("%s: collected out/ entries = %d, want 1", id, len(outs))
		}
		o := outs[0]
		if o.instance != inst.ID || o.run != run || o.session != sess || o.step != 0 || !strings.Contains(o.derived, toolRes.ID) ||
			!strings.Contains(o.args, "out/report.csv") {
			t.Fatalf("%s: out/ provenance %+v (want run %s session %s step 0 derived from %s)", id, o, run, sess, toolRes.ID)
		}
	}
}

// stepArtifactsProbePy: an SDK CognitiveAgent served by the SDK runtime. run()
// writes out/report.csv and uploads "report" — q72_sdk through artifacts.save
// (no lease passed by the author), q72_old through a bare stub with no lease
// metadata (SDK 0.1.6) — and answers with the upload's ledger reference.
const stepArtifactsProbePy = `
import os
from cambrian_agent_sdk import CognitiveAgent
from cambrian_agent_sdk._proto import cambrian_pb2

AGENT = os.environ.get("CAMBRIAN_AGENT_ID", "")

class Step(CognitiveAgent):
    def run(self, task):
        try:
            out = os.environ["CAMBRIAN_AGENT_OUT"]
            os.makedirs(out, exist_ok=True)
            with open(os.path.join(out, "report.csv"), "w", newline="") as f:
                f.write("a,b\n1,2\n")
            if AGENT == "q72_old":
                return self.artifacts._stub.UploadArtifact(cambrian_pb2.UploadArtifactRequest(
                    content=b"report", content_type="text/plain")).hash
            return self.artifacts.save(b"report", content_type="text/plain")
        except Exception as e:
            return "ERROR " + repr(e)

Step(agent_id=AGENT).serve()
`
