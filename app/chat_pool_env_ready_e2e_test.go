package app

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/agentpool"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/health"
	"github.com/cambrian-sh/core/internal/metabolism"
	"github.com/cambrian-sh/core/internal/metabolism/agentmgr"
	"github.com/cambrian-sh/core/internal/substrate/network"
)

// ENG-2105 + ENG-2148 end to end: a first boot whose chat lane starts behind
// the spawn gate — the kernel serving gRPC, then the sandbox Python env ready —
// gets its chat lane without a restart, its workers' first dials are served,
// and health says so throughout.
//
//	chat-pool Lifecycle.Start (chatPoolStarter), as lifecycles start: before
//	  Serve, with the env building → the lane waits on the gate;
//	  /healthz?component=chat 503 "waiting for the kernel to serve"
//	→ the kernel serves (serveMarked) → still waiting: 503 with the env's build
//	  step, grpc cambrian.lane.chat NOT_SERVING, /healthz itself 200; no worker
//	  spawned, no refused launch
//	→ the env (a fake builder) finishes → agentpool.Pool.Start →
//	  AgentManager.SpawnDaemon → REAL SDK workers dial the kernel once each;
//	  the lane turns ready; a turn is served.
//
// Before ENG-2105 the first Start's refusal was only logged, and the lane stayed
// down until a restart ("agent pool not started"); before ENG-2148 the workers
// were spawned before Serve and dialled an unserved backlog.
//
// Needs a Python with the SDK's dependencies (grpcio, protobuf):
//
//	CHATPOOL_E2E_PY   interpreter (a kernel-managed one, or e.g. sdk/.venv)
//	CHATPOOL_E2E_SDK  the SDK checkout under test
func TestE2E_ChatPoolStartsWhenThePythonEnvBecomesReady(t *testing.T) {
	py, sdk := os.Getenv("CHATPOOL_E2E_PY"), os.Getenv("CHATPOOL_E2E_SDK")
	if py == "" || sdk == "" {
		t.Skip("set CHATPOOL_E2E_PY and CHATPOOL_E2E_SDK to run the chat-pool env-ready e2e")
	}
	code := t.TempDir()
	src := strings.Replace(hbProbePy, "@@SDK@@", filepath.ToSlash(sdk), 1)
	if err := os.WriteFile(filepath.Join(code, "chat_probe.py"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := metabolism.NewInMemoryRegistry()
	reg.SetAgent(domain.AgentDefinition{ID: "chat_probe", Runtime: domain.RuntimePython, ExecPath: "chat_probe.py", Dir: code})

	raw, err := net.Listen("tcp", "127.0.0.1:0") // bound early (SEC-03)
	if err != nil {
		t.Fatal(err)
	}
	lis := &acceptCounter{Listener: raw}
	mgr := agentmgr.NewAgentManager(reg, lis.Addr().String(), nil)
	// The fake builder: building until finish(), then the real interpreter.
	env := newDelayedPythonEnv(e2eInterpreter(t, py))
	mgr.SetPythonEnv(env)
	mgr.InstanceManager.SetAgentBootTimeout(60 * time.Second)
	// Heartbeats on, as in production (ADR-0070 D3): each worker opens its
	// heartbeat stream to the kernel as it starts — the boot-time dial that
	// ENG-2148 makes sure lands on a serving kernel.
	mgr.ConfigureHeartbeat(500*time.Millisecond, 3)

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
	pb.RegisterOrchestratorServer(srv, &tokenE2EOrchestrator{Server: &network.Server{SignalReceiver: rec, Heartbeats: mgr}, rec: rec})
	// Not serving yet: the kernel serves from serveMarked, after lifecycles start.
	serving := newServingSignal()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	mgr.StartHeartbeatWatch(ctx)

	// The kernel's readiness (no DB here: process liveness) with the chat lane
	// registered exactly as buildChatLane registers it.
	checker := health.New(nil)
	pool := agentpool.New(mgr, agentpool.Config{AgentID: "chat_probe", Size: 2, StreamPrefix: "chat", AcquireTimeout: 60 * time.Second})
	chatStart := newChatPoolStarter(pool, gateOf(serving, env), env, nil)
	checker.AddComponent(chatLaneComponent, chatStart.Ready)
	lc := Lifecycle{Name: "chat-pool", Start: chatStart.Start, Stop: chatStart.Stop}
	hz := httptest.NewServer(checker.HealthzHandler())
	t.Cleanup(func() {
		lc.Stop()
		hz.Close()
		mgr.StopHeartbeatWatch()
		mgr.StopAllDaemons()
		_ = mgr.InstanceManager.KillAllAgents(context.Background())
		srv.Stop()
	})
	checker.Start(ctx, 100*time.Millisecond)

	get := func(path string) (int, string) {
		t.Helper()
		resp, err := http.Get(hz.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	grpcChat := func() healthpb.HealthCheckResponse_ServingStatus {
		resp, err := checker.Server().Check(ctx, &healthpb.HealthCheckRequest{Service: health.ComponentServicePrefix + chatLaneComponent})
		if err != nil {
			t.Fatal(err)
		}
		return resp.GetStatus()
	}

	// Boot: lifecycles start before Serve, and the env is still building.
	lc.Start(ctx)
	if code, body := get("/healthz?component=chat"); code != http.StatusServiceUnavailable ||
		!strings.Contains(body, "waiting for the kernel to serve") {
		t.Fatalf("chat lane health before the kernel serves: %d %q", code, body)
	}

	// The kernel serves some time later; the env still builds.
	time.Sleep(time.Second)
	go func() { _ = serveMarked(srv, lis, serving) }()
	<-serving.C()
	waitFor(t, ctx, "the chat lane to wait on the env once the kernel serves", func() bool {
		_, body := get("/healthz?component=chat")
		return strings.Contains(body, "waiting for the sandbox Python env")
	})
	if pool.Started() {
		t.Fatal("the pool started while the env was building")
	}
	if code, body := get("/healthz?component=chat"); code != http.StatusServiceUnavailable ||
		!strings.Contains(body, "waiting for the sandbox Python env") || !strings.Contains(body, "step: installing base packages") {
		t.Fatalf("chat lane health while building: %d %q", code, body)
	}
	if code, body := get("/healthz"); code != http.StatusOK || !strings.Contains(body, "chat: not ready") {
		t.Fatalf("/healthz while building (kernel serves, chat not ready): %d %q", code, body)
	}
	waitFor(t, ctx, "grpc cambrian.lane.chat NOT_SERVING", func() bool {
		return grpcChat() == healthpb.HealthCheckResponse_NOT_SERVING
	})
	if _, err := pool.Dispatch(ctx, hbHandoff("early")); err == nil {
		t.Fatal("a turn was served before the pool started")
	}

	// The build finishes some time later; nothing restarts the kernel.
	time.Sleep(time.Second)
	env.finish()
	waitFor(t, ctx, "the chat pool to start on the env's readiness signal", pool.Started)
	if code, body := get("/healthz?component=chat"); code != http.StatusOK || body != "chat: ready\n" {
		t.Fatalf("chat lane health once up: %d %q", code, body)
	}
	waitFor(t, ctx, "grpc cambrian.lane.chat SERVING", func() bool {
		return grpcChat() == healthpb.HealthCheckResponse_SERVING
	})
	if got := turn(t, ctx, pool, "hello", 60*time.Second); got != "pong:hello" {
		t.Fatalf("first turn after the env became ready: %q", got)
	}
	// ENG-2148: each worker was spawned after Serve, so its first dial — the
	// heartbeat stream it opens at start — was served: both are beating, over
	// one connection each, none abandoned in the backlog and redialled (a
	// redial shows up as an extra accepted connection).
	for _, stream := range []string{"chat-chat_probe-0", "chat-chat_probe-1"} {
		waitFor(t, ctx, stream+"'s first heartbeat", func() bool {
			tracked, _ := mgr.DaemonServing(stream, time.Time{})
			return tracked
		})
	}
	if n := lis.accepted(); n != 2 {
		t.Fatalf("kernel accepted %d connections from 2 chat workers; want exactly one dial each", n)
	}
}

// acceptCounter counts the connections the kernel's gRPC server accepts.
type acceptCounter struct {
	net.Listener
	n atomic.Int32
}

func (l *acceptCounter) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.n.Add(1)
	}
	return c, err
}

func (l *acceptCounter) accepted() int32 { return l.n.Load() }
