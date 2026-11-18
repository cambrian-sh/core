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

// ENG-2105 end to end: a first boot whose sandbox Python env becomes ready only
// after a delay gets its chat lane without a restart, and health says so.
//
//	chat-pool Lifecycle.Start (chatPoolStarter) → agentpool.Pool.Start →
//	  AgentManager.SpawnDaemon → the launch gate refuses: pyenv.ErrBuilding
//	  → the lane waits on the env's ReadyC; /healthz?component=chat 503 with the
//	  build step, grpc cambrian.lane.chat NOT_SERVING, /healthz itself 200
//	→ the env (a fake builder) finishes → the pool spawns REAL SDK workers →
//	  the lane turns ready; a turn is served.
//
// Before the fix the first Start's refusal was only logged, and the lane stayed
// down until a restart ("agent pool not started").
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

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mgr := agentmgr.NewAgentManager(reg, lis.Addr().String(), nil)
	// The fake builder: building until finish(), then the real interpreter.
	env := newDelayedPythonEnv(e2eInterpreter(t, py))
	mgr.SetPythonEnv(env)
	mgr.InstanceManager.SetAgentBootTimeout(60 * time.Second)

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
	pb.RegisterOrchestratorServer(srv, &tokenE2EOrchestrator{Server: &network.Server{SignalReceiver: rec}, rec: rec})
	go func() { _ = srv.Serve(lis) }()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// The kernel's readiness (no DB here: process liveness) with the chat lane
	// registered exactly as buildChatLane registers it.
	checker := health.New(nil)
	pool := agentpool.New(mgr, agentpool.Config{AgentID: "chat_probe", Size: 2, StreamPrefix: "chat", AcquireTimeout: 60 * time.Second})
	chatStart := newChatPoolStarter(pool, env, nil)
	checker.AddComponent(chatLaneComponent, chatStart.Ready)
	lc := Lifecycle{Name: "chat-pool", Start: chatStart.Start, Stop: chatStart.Stop}
	hz := httptest.NewServer(checker.HealthzHandler())
	t.Cleanup(func() {
		lc.Stop()
		hz.Close()
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

	// Boot: the env is still building.
	lc.Start(ctx)
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
}
