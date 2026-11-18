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

	"google.golang.org/grpc"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/agentpool"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/metabolism"
	"github.com/cambrian-sh/core/internal/metabolism/agentmgr"
	"github.com/cambrian-sh/core/internal/pyenv"
	"github.com/cambrian-sh/core/internal/substrate/network"
)

// ADR-0070 D3 / ENG-1348 end to end: heartbeat liveness reclaims a chat pool worker
// that stays alive but stops serving, through the REAL path with REAL Python SDK
// workers:
//
//	agentpool.Pool → AgentManager.SpawnDaemon → bootDaemonAgent (--heartbeat-interval-ms,
//	  per-instance token) → the SDK's start_agent_server opens a heartbeat-only
//	  SignalStream → the kernel's gRPC server with the production agent-credential
//	  interceptors → network.Server.SignalStream → AgentManager.RecordDaemonHeartbeat
//	  (instance from the token) → the heartbeat watch → kill → restart policy.
//
// It reproduces the readiness finding (ENG-1995 phase B block 6): a pool of ONE worker
// whose Execute hangs (time.sleep) after the turn was cut loose. Before, the turn
// stalled and the NEXT turn on the pool stalled too, because the only worker was still
// busy. Asserted now: the hung worker is killed and replaced, and the next turn,
// sent right after the first was abandoned, succeeds on the fresh worker.
//
// And the compatibility half: a worker on an OLDER SDK (no liveness module, sends no
// beats) is never tracked and never killed, however long it stays silent.
//
// Needs a Python with the SDK's dependencies (grpcio, protobuf):
//
//	HEARTBEAT_E2E_PY       interpreter (a kernel-managed one, or e.g. sdk/.venv)
//	HEARTBEAT_E2E_SDK      the SDK checkout under test (0.1.8+, with liveness.py)
//	HEARTBEAT_E2E_OLD_SDK  an SDK checkout from before heartbeats (0.1.7)
func TestE2E_HungPoolWorkerIsReclaimedByHeartbeat(t *testing.T) {
	py, newSDK, oldSDK := os.Getenv("HEARTBEAT_E2E_PY"), os.Getenv("HEARTBEAT_E2E_SDK"), os.Getenv("HEARTBEAT_E2E_OLD_SDK")
	if py == "" || newSDK == "" || oldSDK == "" {
		t.Skip("set HEARTBEAT_E2E_PY, HEARTBEAT_E2E_SDK and HEARTBEAT_E2E_OLD_SDK to run the heartbeat liveness e2e")
	}
	if _, err := os.Stat(filepath.Join(newSDK, "cambrian_agent_sdk", "liveness.py")); err != nil {
		t.Fatalf("HEARTBEAT_E2E_SDK has no liveness.py: %v", err)
	}
	if _, err := os.Stat(filepath.Join(oldSDK, "cambrian_agent_sdk", "liveness.py")); err == nil {
		t.Fatal("HEARTBEAT_E2E_OLD_SDK already has liveness.py; point it at an SDK from before heartbeats")
	}

	code := t.TempDir()
	writeProbe := func(name, sdk string) {
		src := strings.Replace(hbProbePy, "@@SDK@@", filepath.ToSlash(sdk), 1)
		if err := os.WriteFile(filepath.Join(code, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeProbe("hb_worker.py", newSDK)
	writeProbe("hb_old.py", oldSDK)

	reg := metabolism.NewInMemoryRegistry()
	worker := domain.AgentDefinition{ID: "hb_worker", Runtime: domain.RuntimePython, ExecPath: "hb_worker.py", Dir: code}
	old := domain.AgentDefinition{ID: "hb_old", Runtime: domain.RuntimePython, ExecPath: "hb_old.py", Dir: code}
	reg.SetAgent(worker)
	reg.SetAgent(old)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mgr := agentmgr.NewAgentManager(reg, lis.Addr().String(), nil)
	mgr.SetPythonEnv(fixedPythonEnv{pi: e2eInterpreter(t, py)})
	mgr.InstanceManager.SetAgentBootTimeout(60 * time.Second)

	// The production settings, scaled down: beats every 500 ms, 3 missed = hung.
	agents := config.AgentsConfig{
		DaemonHeartbeatIntervalMs: 500, DaemonHeartbeatMissedBeforeKill: 3,
		DaemonRestartMaxAttempts: 5, DaemonRestartWindowSeconds: 60,
		DaemonRestartBaseBackoffMs: 50, DaemonRestartMaxBackoffMs: 200,
		AgentBootTimeoutMs: 60000,
	}
	mgr.RestartPolicy = agentmgr.NewDaemonRestartPolicy(agents.DaemonRestartMaxAttempts,
		time.Minute, 50*time.Millisecond, 200*time.Millisecond)
	mgr.ConfigureHeartbeat(500*time.Millisecond, agents.DaemonHeartbeatMissedBeforeKill)
	bus := newHBBus()
	mgr.EventBus = bus

	verifier, err := authz.NewAgentCredentialVerifier(
		func(string) (string, bool) { return "", false }, mgr.InstanceManager.Tokens(), kernelSpawnedAgent(reg), false)
	if err != nil {
		t.Fatal(err)
	}
	rec := newTokenRecorder()
	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(authz.UnarySurfaceInterceptor(nil), authz.UnaryAgentAuthInterceptor(verifier, nil)),
		grpc.ChainStreamInterceptor(authz.StreamSurfaceInterceptor(nil), authz.StreamAgentAuthInterceptor(verifier, nil)),
	)
	pb.RegisterOrchestratorServer(srv, &tokenE2EOrchestrator{
		Server: &network.Server{SignalReceiver: rec, Heartbeats: mgr}, rec: rec})
	go func() { _ = srv.Serve(lis) }()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	mgr.StartHeartbeatWatch(ctx)

	pool := agentpool.New(mgr, agentpool.Config{
		AgentID: "hb_worker", Size: 1, StreamPrefix: "chat",
		AcquireTimeout: 90 * time.Second, RecoverTimeout: poolRecoverTimeout(agents),
	})
	oldPool := agentpool.New(mgr, agentpool.Config{
		AgentID: "hb_old", Size: 1, StreamPrefix: "chat",
		AcquireTimeout: 90 * time.Second, RecoverTimeout: poolRecoverTimeout(agents),
	})
	t.Cleanup(func() {
		pool.Stop()
		oldPool.Stop()
		mgr.StopHeartbeatWatch()
		mgr.StopAllDaemons()
		_ = mgr.InstanceManager.KillAllAgents(context.Background())
		srv.Stop()
	})
	if err := pool.Start(ctx); err != nil {
		t.Fatalf("pool start: %v", err)
	}
	if err := oldPool.Start(ctx); err != nil {
		t.Fatalf("old-SDK pool start: %v", err)
	}
	const stream, oldStream = "chat-hb_worker-0", "chat-hb_old-0"

	// The new-SDK worker arms tracking with its first beat.
	waitFor(t, ctx, "the new-SDK worker's first heartbeat", func() bool {
		tracked, _ := mgr.DaemonServing(stream, time.Time{})
		return tracked
	})
	hung, _ := mgr.DaemonInstanceID(stream)

	// A normal turn works.
	if got := turn(t, ctx, pool, "hello", 30*time.Second); got != "pong:hello" {
		t.Fatalf("first normal turn: %q", got)
	}

	// Turn 1 hangs inside Execute; the caller cuts it loose (the chat stall watcher's
	// cancel), the worker process keeps running its time.sleep.
	cutCtx, cut := context.WithTimeout(ctx, 2*time.Second)
	_, err = pool.Dispatch(cutCtx, hbHandoff("hang"))
	cut()
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "DeadlineExceeded") {
		t.Fatalf("hung turn: %v, want the caller's deadline", err)
	}
	abandoned := time.Now()

	// Turn 2, sent at once: before ENG-1348 it queued behind the hung worker and
	// stalled too. Now it waits for the replacement and succeeds.
	if got := turn(t, ctx, pool, "again", 90*time.Second); got != "pong:again" {
		t.Fatalf("next turn after the hang: %q", got)
	}
	t.Logf("next turn served %s after the hung turn was abandoned", time.Since(abandoned).Round(time.Millisecond))
	replaced, ok := mgr.DaemonInstanceID(stream)
	if !ok || replaced == hung {
		t.Fatalf("the next turn ran on the hung instance %q (now %q, live=%v)", hung, replaced, ok)
	}
	if _, still := mgr.InstanceManager.GetInstanceByID(hung); still {
		t.Fatal("the hung instance is still registered")
	}
	if bus.count(domain.EventTypeDaemonCrashed) < 1 || bus.count(domain.EventTypeDaemonRecovered) < 1 {
		t.Fatalf("operator feed: crashed=%d recovered=%d, want both", bus.count(domain.EventTypeDaemonCrashed),
			bus.count(domain.EventTypeDaemonRecovered))
	}

	// The old-SDK worker sent no beat in all that time and was never touched.
	oldInst, ok := mgr.DaemonInstanceID(oldStream)
	if !ok {
		t.Fatal("old-SDK worker not live")
	}
	if tracked, _ := mgr.DaemonServing(oldStream, time.Time{}); tracked {
		t.Fatal("an old-SDK worker was heartbeat-tracked: it cannot beat")
	}
	// Even silent with an abandoned hung turn, well past the missed-beat budget.
	cutCtx, cut = context.WithTimeout(ctx, time.Second)
	_, _ = oldPool.Dispatch(cutCtx, hbHandoff("hang"))
	cut()
	time.Sleep(4 * time.Second) // 8 intervals > 3 missed
	if now, ok := mgr.DaemonInstanceID(oldStream); !ok || now != oldInst {
		t.Fatalf("the old-SDK worker was killed for not heartbeating (was %q, now %q live=%v)", oldInst, now, ok)
	}
}

func e2eInterpreter(t *testing.T, py string) pyenv.Interpreter {
	t.Helper()
	abs, err := filepath.Abs(py)
	if err != nil {
		t.Fatal(err)
	}
	for _, home := range []string{filepath.Dir(abs), filepath.Dir(filepath.Dir(abs))} {
		if _, err := os.Stat(filepath.Join(home, "generation.json")); err == nil {
			return managedInterpreter(t, py)
		}
	}
	// Unsandboxed, so any interpreter with the SDK's dependencies runs the probes.
	t.Logf("using unmanaged interpreter %s (unsandboxed launch)", abs)
	return pyenv.Interpreter{Path: abs}
}

func hbHandoff(text string) *domain.Handoff {
	return &domain.Handoff{Payload: &domain.Payload{Type: "text", Data: []byte(text)}}
}

func turn(t *testing.T, ctx context.Context, p *agentpool.Pool, text string, d time.Duration) string {
	t.Helper()
	tctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	resp, err := p.Dispatch(tctx, hbHandoff(text))
	if err != nil {
		t.Fatalf("turn %q: %v", text, err)
	}
	if resp == nil || resp.Payload == nil {
		return ""
	}
	return string(resp.Payload.Data)
}

func waitFor(t *testing.T, ctx context.Context, what string, cond func() bool) {
	t.Helper()
	for !cond() {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

type hbBus struct {
	rec *tokenRecorder
	n   map[string]int
}

func newHBBus() *hbBus { return &hbBus{rec: newTokenRecorder(), n: map[string]int{}} }

func (b *hbBus) Publish(_ context.Context, e domain.DomainEvent) error {
	b.rec.mu.Lock()
	b.n[e.EventType()]++
	b.rec.mu.Unlock()
	return nil
}

func (b *hbBus) count(typ string) int {
	b.rec.mu.Lock()
	defer b.rec.mu.Unlock()
	return b.n[typ]
}

// hbProbePy is a chat-pool-style worker: a CognitiveAgent served by the SDK's own
// runtime (agent.serve()), whose run() answers "pong:<text>" — or, for "hang", never
// returns (the readiness repro: time.sleep inside Execute). @@SDK@@ pins which SDK
// checkout it runs on.
const hbProbePy = `
import sys, time
sys.path.insert(0, "@@SDK@@")
from cambrian_agent_sdk import CognitiveAgent

class Probe(CognitiveAgent):
    def run(self, task):
        if task.text == "hang":
            time.sleep(3600)
        return "pong:" + task.text

if __name__ == "__main__":
    Probe(agent_id="hb_probe").serve()
`
