package app

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/metabolism"
	"github.com/cambrian-sh/core/internal/metabolism/agentmgr"
	"github.com/cambrian-sh/core/internal/substrate/network"
	supwatcher "github.com/cambrian-sh/core/internal/supervision/watcher"
)

// ADR-0157 S3 end to end, per-instance agent tokens, through the REAL boot
// path with a REAL Python SDK agent:
//
//	InstanceManager.GetOrBootInstance → bootAgentInstance (mint, env only)
//	  → the agent's SDK channel (cambrian_agent_sdk.channel.build_channel reads
//	    CAMBRIAN_AGENT_TOKEN / CAMBRIAN_AGENT_ID)
//	  → the kernel's gRPC server with the production surface + agent-credential
//	    interceptors and the production verifier (authz.NewAgentCredentialVerifier
//	    over the manager's token registry and kernelSpawnedAgent(registry))
//	  → network.Server.SignalStream gated by the OSS Watcher (signal.ValidateToken
//	    → FindInstanceByToken) for the daemon's signals.
//
// Asserted (ADR-0157 S3 gate):
//   - a live spawned agent's RPC is admitted as its own principal, and a daemon
//     instance's signals pass the SignalStream gate and arrive;
//   - after the instance is restarted, a live SDK process presenting the OLD
//     token is refused Unauthenticated, while the new instance is admitted;
//   - an external SDK agent (not kernel-spawned) still authenticates with its
//     stored secret, and a kernel-spawned agent id is refused on a stored secret.
//
// Needs a Python with the agent SDK:
//
//	TOKEN_E2E_PY       the interpreter (e.g. sdk/.venv/Scripts/python.exe)
//	TOKEN_E2E_SANDBOX  (Linux, bubblewrap) non-empty: launch under the ADR-0157
//	                   sandbox as third-party agents, so each instance is
//	                   kernel-only and authenticates over its per-instance
//	                   kernel unix socket (S2) through the same verifier
//
// The tokens are compared, never printed.
func TestE2E_PerInstanceAgentTokens(t *testing.T) {
	py := os.Getenv("TOKEN_E2E_PY")
	if py == "" {
		t.Skip("set TOKEN_E2E_PY to a Python with the agent SDK to run the per-instance token e2e")
	}
	sandboxed := os.Getenv("TOKEN_E2E_SANDBOX") != ""
	if sandboxed && runtime.GOOS != "linux" {
		t.Skip("TOKEN_E2E_SANDBOX needs Linux with bubblewrap")
	}
	base := t.TempDir()
	if sandboxed {
		// As root the third-party agent drops to uid 65534, which must reach its
		// code and work dirs: t.TempDir's parent is 0700, so use a 0755 dir.
		var err error
		if base, err = os.MkdirTemp("", "s3e"); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(base, 0o755)
		t.Cleanup(func() { _ = os.RemoveAll(base) })
	}
	code := filepath.Join(base, "code")
	if err := os.MkdirAll(code, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(code, "s3_probe.py"), []byte(tokenProbePy), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := metabolism.NewInMemoryRegistry()
	spawned := &domain.AgentDefinition{ID: "s3_probe", Runtime: domain.RuntimePython, ExecPath: "s3_probe.py", Dir: code}
	daemon := &domain.AgentDefinition{ID: "s3_daemon", Runtime: domain.RuntimePython, ExecPath: "s3_probe.py", Dir: code,
		Trait: domain.TraitDaemon}
	if sandboxed {
		spawned.TrustTier, daemon.TrustTier = "third-party", "third-party"
	}
	reg.SetAgent(*spawned)
	reg.SetAgent(*daemon)
	stored := map[string]string{
		authz.AgentTokenSecretName("external_sdk"): "external-stored-secret",
		authz.AgentTokenSecretName("s3_probe"):     "stale-stored-secret", // must be refused
	}

	// The kernel's TCP plane.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mgr := agentmgr.NewAgentManager(reg, py, lis.Addr().String(), nil)
	im := mgr.InstanceManager
	im.SetAgentBootTimeout(60 * time.Second)
	sockets := newAgentSocketServer()
	if sandboxed {
		root := base
		kernelDir := filepath.Join(root, "kernel")
		if err := os.MkdirAll(filepath.Join(kernelDir, "configs"), 0o755); err != nil {
			t.Fatal(err)
		}
		sb := agentmgr.NewAgentSandbox(agentmgr.AgentSandboxOptions{
			WorkRoot: filepath.Join(root, "w"), KernelDir: kernelDir,
			KernelStatePaths: []string{filepath.Join(kernelDir, "configs")},
		})
		im.SetAgentContainment(agentmgr.AgentContainment{Sandbox: sb, ThirdPartyDegrade: os.Geteuid() != 0})
		im.SetAgentTransport(agentmgr.AgentTransport{KernelSocket: sockets.Open})
	}
	verifier, err := authz.NewAgentCredentialVerifier(
		func(n string) (string, bool) { v, ok := stored[n]; return v, ok },
		im.Tokens(), kernelSpawnedAgent(reg), false)
	if err != nil {
		t.Fatal(err)
	}
	rec := newTokenRecorder()
	w := supwatcher.New(mgr, nil, nil, nil, nil, supwatcher.WatcherConfig{})
	orch := &tokenE2EOrchestrator{Server: &network.Server{Watcher: w, SignalReceiver: rec}, rec: rec}
	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(authz.UnarySurfaceInterceptor(nil), authz.UnaryAgentAuthInterceptor(verifier, nil)),
		grpc.ChainStreamInterceptor(authz.StreamSurfaceInterceptor(nil), authz.StreamAgentAuthInterceptor(verifier, nil)),
	)
	pb.RegisterOrchestratorServer(srv, orch)
	sockets.start(srv) // the kernel-only agents' unix sockets, on the same server
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		mgr.StopAllDaemons()
		_ = im.KillAllAgents(context.Background()) // best-effort teardown
		srv.Stop()
		sockets.stop()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// 1. A live spawned agent's RPC is admitted as itself.
	first, err := im.GetOrBootInstance(ctx, spawned, "")
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	if !rec.waitPrincipal(ctx, "s3_probe", 1) {
		t.Fatal("the spawned agent's ListTools never reached the kernel as s3_probe")
	}
	oldTok := readSideChannelToken(t, first)
	if sandboxed {
		// The probe recorded the --substrate-addr it was given: a kernel-only
		// instance must have authenticated over its unix kernel socket.
		if addr := readSideChannel(t, first, ".addr"); !strings.HasPrefix(addr, "unix:") {
			t.Fatalf("sandboxed third-party instance dialled %q, want its unix kernel socket", addr)
		}
	}

	// 2. Restart the instance; the old token is dead, the new instance works.
	im.EvictInstance(first.ID)
	second, err := im.GetOrBootInstance(ctx, spawned, "")
	if err != nil {
		t.Fatalf("reboot: %v", err)
	}
	if !rec.waitPrincipal(ctx, "s3_probe", 2) {
		t.Fatal("the restarted instance's ListTools was not admitted")
	}
	if newTok := readSideChannelToken(t, second); newTok == oldTok {
		t.Fatal("the restarted instance received the old token")
	}
	if got := handStartedCall(t, ctx, py, code, lis.Addr().String(), "s3_probe", oldTok); got != "UNAUTHENTICATED" {
		t.Fatalf("a live RPC with the restarted instance's OLD token: %s, want UNAUTHENTICATED", got)
	}

	// 3. External agent with its stored secret: admitted. A kernel-spawned id on a
	//    stored secret: refused.
	if got := handStartedCall(t, ctx, py, code, lis.Addr().String(), "external_sdk", "external-stored-secret"); got != "OK" {
		t.Fatalf("external SDK agent with its stored secret: %s, want OK", got)
	}
	if got := handStartedCall(t, ctx, py, code, lis.Addr().String(), "s3_probe", "stale-stored-secret"); got != "UNAUTHENTICATED" {
		t.Fatalf("kernel-spawned agent on a stored secret: %s, want UNAUTHENTICATED", got)
	}

	// 4. A daemon instance's signals pass the SignalStream gate and arrive.
	if _, err := mgr.SpawnDaemon(ctx, daemon.ID, "s3-stream", nil); err != nil {
		t.Fatalf("daemon boot: %v", err)
	}
	if !rec.waitSignal(ctx, "s3_daemon") {
		t.Fatal("no daemon signal arrived through the SignalStream gate")
	}
}

// tokenE2EOrchestrator is the real network.Server (SignalStream with the OSS
// Watcher gate) with ListTools recording the authenticated principal.
type tokenE2EOrchestrator struct {
	*network.Server
	rec *tokenRecorder
}

func (o *tokenE2EOrchestrator) ListTools(ctx context.Context, _ *pb.ListToolsRequest) (*pb.ListToolsResponse, error) {
	o.rec.principal(domain.PrincipalFromContext(ctx).ID)
	return &pb.ListToolsResponse{}, nil
}

type tokenRecorder struct {
	mu         sync.Mutex
	principals map[string]int
	signals    map[string]int
}

func newTokenRecorder() *tokenRecorder {
	return &tokenRecorder{principals: map[string]int{}, signals: map[string]int{}}
}

func (r *tokenRecorder) principal(id string) {
	r.mu.Lock()
	r.principals[id]++
	r.mu.Unlock()
}

func (r *tokenRecorder) OnSignal(_ context.Context, s domain.Signal) error {
	r.mu.Lock()
	r.signals[s.FromAgent]++
	r.mu.Unlock()
	return nil
}

func (r *tokenRecorder) waitPrincipal(ctx context.Context, id string, n int) bool {
	return r.wait(ctx, func() bool { return r.principals[id] >= n })
}

func (r *tokenRecorder) waitSignal(ctx context.Context, from string) bool {
	return r.wait(ctx, func() bool { return r.signals[from] > 0 })
}

func (r *tokenRecorder) wait(ctx context.Context, done func() bool) bool {
	for {
		r.mu.Lock()
		ok := done()
		r.mu.Unlock()
		if ok {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// readSideChannelToken reads the token the probe wrote next to its socket (a
// test-only side channel, 0600, deleted at once) so the test can replay it.
func readSideChannelToken(t *testing.T, inst *domain.Instance) string {
	t.Helper()
	return readSideChannel(t, inst, ".tok")
}

func readSideChannel(t *testing.T, inst *domain.Instance, suffix string) string {
	t.Helper()
	p := inst.SocketPath + suffix
	deadline := time.Now().Add(30 * time.Second)
	for {
		b, err := os.ReadFile(p)
		if err == nil && len(b) > 0 {
			_ = os.Remove(p)
			return strings.TrimSpace(string(b))
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe did not write its token side channel: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// handStartedCall runs the probe as an agent started BY HAND (not by the
// kernel) with the given credential in its environment, and returns its
// ListTools outcome ("OK" or the gRPC status code name).
func handStartedCall(t *testing.T, ctx context.Context, py, dir, addr, agentID, token string) string {
	t.Helper()
	cmd := exec.CommandContext(ctx, py, "s3_probe.py", "--mode", "call", "--substrate-addr", addr)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CAMBRIAN_AGENT_TOKEN="+token, "CAMBRIAN_AGENT_ID="+agentID, "PYTHONUTF8=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("hand-started probe failed: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if r, ok := strings.CutPrefix(strings.TrimSpace(line), "RESULT "); ok {
			return r
		}
	}
	t.Fatalf("hand-started probe printed no RESULT line")
	return ""
}

// tokenProbePy is the probe agent. Spawned by the kernel it serves --socket,
// writes its token to <socket>.tok for the test (never prints it), calls
// ListTools through the SDK channel, and — launched as a daemon — runs the
// SDK's own daemon runtime (start_daemon), which opens the SignalStream and
// emits one signal. With --mode call it is an agent started by hand: one
// ListTools with the credential in its environment, then RESULT <outcome>.
const tokenProbePy = `
import os, sys, time
from concurrent import futures
import grpc
from cambrian_agent_sdk import DaemonAgent
from cambrian_agent_sdk.channel import build_channel
from cambrian_agent_sdk._proto import cambrian_pb2, cambrian_pb2_grpc

def arg(name):
    a = sys.argv
    return a[a.index(name) + 1] if name in a and a.index(name) + 1 < len(a) else ""

addr = arg("--substrate-addr")

def list_tools():
    stub = cambrian_pb2_grpc.OrchestratorStub(build_channel(addr))
    try:
        stub.ListTools(cambrian_pb2.ListToolsRequest(), timeout=15)
        return "OK"
    except grpc.RpcError as e:
        return e.code().name

if arg("--mode") == "call":
    print("RESULT " + list_tools(), flush=True)
    sys.exit(0)

sock = arg("--socket")
server = grpc.server(futures.ThreadPoolExecutor(max_workers=2))
server.add_insecure_port("unix:" + sock)
server.start()
with open(sock + ".addr", "w") as f:
    f.write(addr)
with open(sock + ".tok", "w") as f:
    f.write(os.environ.get("CAMBRIAN_AGENT_TOKEN", ""))

if "--daemon-mode" in sys.argv:
    class Probe(DaemonAgent):
        def daemon_loop(self):
            self.send_signal({"probe": "s3"}, raw_text="s3 probe signal")
            while True:
                time.sleep(1)
    Probe(agent_id=arg("--agent-id")).serve(addr)
else:
    list_tools()
    server.wait_for_termination()
`
