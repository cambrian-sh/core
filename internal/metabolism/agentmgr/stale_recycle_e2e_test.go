package agentmgr

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/metabolism"
)

// execProbe is an agent that serves Execute: it answers with its pid, after
// sleeping the handoff's sleep_ms (so a test can hold a turn in flight).
type execProbe struct {
	pb.UnimplementedAgentServiceServer
}

func (execProbe) Execute(_ context.Context, h *pb.Handoff) (*pb.Handoff, error) {
	if ms, _ := strconv.Atoi(h.GetMetadata()["sleep_ms"]); ms > 0 {
		time.Sleep(time.Duration(ms) * time.Millisecond)
	}
	return &pb.Handoff{Metadata: map[string]string{"pid": strconv.Itoa(os.Getpid())}}, nil
}

// runExecProbeAgent is the execprobe agent (TestMain, AGENTMGR_CHAOS_HELPER=execprobe).
func runExecProbeAgent() {
	var sock string
	for i, a := range os.Args {
		if a == "--socket" && i+1 < len(os.Args) {
			sock = os.Args[i+1]
		}
	}
	lis, err := net.Listen("unix", sock)
	if err != nil {
		os.Exit(3)
	}
	srv := grpc.NewServer()
	healthpb.RegisterHealthServer(srv, health.NewServer())
	pb.RegisterAgentServiceServer(srv, execProbe{})
	_ = srv.Serve(lis)
}

// ADR-0157 D7.10.2 (P4) through the real launch and dispatch path: real agent
// processes booted by the instance manager, real Execute RPCs. A layer change
// (MarkAgentStale) during a turn does not kill the turn; the instance is
// recycled when the turn ends and the next turn runs on a new process. A pool
// worker (daemon) is recycled onto its stream, and the next turn waits for it.
func TestE2E_StaleInstancesRecycledWhenIdle(t *testing.T) {
	if testing.Short() {
		t.Skip("launches real subprocesses; skipped in -short")
	}
	root := e2eRoot(t)
	reg := metabolism.NewInMemoryRegistry()
	exe := probeExe(t, root)
	reg.SetAgent(domain.AgentDefinition{ID: "analyst", Name: "analyst", Runtime: domain.RuntimeBinary, ExecPath: exe,
		Dir: filepath.Dir(exe), TrustTier: "first-party"})
	reg.SetAgent(domain.AgentDefinition{ID: "chat", Name: "chat", Runtime: domain.RuntimeBinary, ExecPath: exe,
		Dir: filepath.Dir(exe), TrustTier: "first-party"})
	m := NewAgentManager(reg, "127.0.0.1:1", nil)
	sb := NewAgentSandbox(AgentSandboxOptions{WorkRoot: filepath.Join(root, "w"), KernelDir: filepath.Join(root, "kernel")})
	m.SetAgentContainment(AgentContainment{Sandbox: sb})
	m.SetAgentTransport(AgentTransport{KernelSocket: listenKernelSocketForTest})
	m.SetEnvExtras([]string{"AGENTMGR_CHAOS_HELPER=execprobe"})
	m.SetAgentBootTimeout(60 * time.Second)
	t.Cleanup(func() { m.StopAllDaemons(); m.EvictAgent("analyst"); m.EvictAgent("chat") })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pidOf := func(h *domain.Handoff, err error) string {
		t.Helper()
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		return h.Context["pid"]
	}

	// JIT: a shared running instance, reused by CallAgent.
	def, _ := reg.GetAgentByName(ctx, "analyst")
	first, err := m.GetOrBootInstance(ctx, def, "")
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	p1 := pidOf(m.CallAgent(ctx, "analyst", &domain.Handoff{}))
	type res struct {
		pid string
		err error
	}
	inFlight := make(chan res, 1)
	go func() {
		h, err := m.CallAgent(ctx, "analyst", &domain.Handoff{Context: map[string]string{"sleep_ms": "800"}})
		if err != nil {
			inFlight <- res{err: err}
			return
		}
		inFlight <- res{pid: h.Context["pid"]}
	}()
	time.Sleep(200 * time.Millisecond) // the turn is running
	m.MarkAgentStale("analyst")
	if _, ok := m.GetInstanceByID(first.ID); !ok {
		t.Fatal("the busy instance must not be killed when it goes stale")
	}
	r := <-inFlight
	if r.err != nil || r.pid != p1 {
		t.Fatalf("the turn in flight must finish on its own process: %+v (pid %s)", r, p1)
	}
	if _, ok := m.GetInstanceByID(first.ID); ok {
		t.Fatal("the stale instance must be recycled once its turn ended")
	}
	next, err := m.GetOrBootInstance(ctx, def, "")
	if err != nil {
		t.Fatal(err)
	}
	if next.ID == first.ID {
		t.Fatal("the next turn must get a new instance")
	}
	if p2 := pidOf(m.CallAgent(ctx, "analyst", &domain.Handoff{})); p2 == p1 || p2 == "" {
		t.Fatalf("the next turn must run on a new process: %s vs %s", p2, p1)
	}

	// Daemon (a chat pool worker): idle when marked ⇒ recycled at once onto
	// the same stream; the next turn waits for the replacement.
	if _, err := m.SpawnDaemon(ctx, "chat", "pool-0", nil); err != nil {
		t.Fatalf("spawn daemon: %v", err)
	}
	w1, _ := m.DaemonInstanceID("pool-0")
	d1 := pidOf(m.CallDaemon(ctx, "pool-0", &domain.Handoff{}))
	m.MarkAgentStale("chat")
	d2 := pidOf(m.CallDaemon(ctx, "pool-0", &domain.Handoff{}))
	w2, _ := m.DaemonInstanceID("pool-0")
	if d2 == d1 || w2 == w1 || w2 == "" {
		t.Fatalf("the stream must be served by a new worker process: pid %s→%s, instance %s→%s", d1, d2, w1, w2)
	}
	if got := m.DaemonRefCount("pool-0"); got != 1 {
		t.Fatalf("the recycle keeps the stream's references: %d", got)
	}
	if st := m.GetDaemonStatus("pool-0"); st != "running" {
		t.Fatalf("status after the recycle: %q", st)
	}
}
