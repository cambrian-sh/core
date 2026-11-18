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
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/metabolism"
	"github.com/cambrian-sh/core/internal/metabolism/agentmgr"
	"github.com/cambrian-sh/core/internal/pyenv"
	"github.com/cambrian-sh/core/internal/substrate/network"
)

// ENG-2137 end to end: a first boot whose sandbox Python env becomes ready
// only after the kernel serves. A plugin's boot-time daemon spawn (the premium
// Telegram restore, a reactive watch's stream daemon) waits on the ONE gate,
// KernelServices.AwaitSpawnReady, wired as buildKernelServices wires it:
//
//	Serving = the serving signal serveMarked closes right before Serve
//	PythonEnv = pythonSpawnReadinessOf(the kernel's env gate)
//
// The kernel serves; the gated spawn still waits (the env builds); the env (a
// fake builder) finishes; the spawn goes through the REAL AgentManager launch
// path and an SDK daemon comes up — once, with no restart and no refused
// attempt. The ungated control spawned at the same moment is refused with
// pyenv.ErrBuilding, which is what left the Telegram ingress and watch daemons
// off until a restart.
//
// Needs a Python with the SDK's dependencies (grpcio, protobuf):
//
//	CHATPOOL_E2E_PY   interpreter (a kernel-managed one, or e.g. sdk/.venv)
//	CHATPOOL_E2E_SDK  the SDK checkout under test
func TestE2E_PluginDaemonSpawnWaitsForThePythonEnv(t *testing.T) {
	py, sdk := os.Getenv("CHATPOOL_E2E_PY"), os.Getenv("CHATPOOL_E2E_SDK")
	if py == "" || sdk == "" {
		t.Skip("set CHATPOOL_E2E_PY and CHATPOOL_E2E_SDK to run the plugin spawn-gate e2e")
	}
	code := t.TempDir()
	src := strings.Replace(hbProbePy, "@@SDK@@", filepath.ToSlash(sdk), 1)
	if err := os.WriteFile(filepath.Join(code, "ingress_probe.py"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := metabolism.NewInMemoryRegistry()
	reg.SetAgent(domain.AgentDefinition{ID: "ingress_probe", Runtime: domain.RuntimePython, ExecPath: "ingress_probe.py", Dir: code})

	lis, err := net.Listen("tcp", "127.0.0.1:0") // bound early (SEC-03)
	if err != nil {
		t.Fatal(err)
	}
	mgr := agentmgr.NewAgentManager(reg, lis.Addr().String(), nil)
	env := newDelayedPythonEnv(e2eInterpreter(t, py)) // building until finish()
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
	t.Cleanup(func() {
		mgr.StopAllDaemons()
		_ = mgr.InstanceManager.KillAllAgents(context.Background())
		srv.Stop()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// What buildKernelServices hands every plugin.
	serving := newServingSignal()
	svc := KernelServices{Serving: serving.C(), PythonEnv: pythonSpawnReadinessOf(env)}
	if svc.PythonEnv.Ready == nil || svc.PythonEnv.Settled == nil {
		t.Fatal("a managed env must reach the plugins' spawn gate")
	}

	// The plugin's boot-time spawn, started as lifecycles start: before Serve.
	type result struct {
		inst string
		err  error
	}
	gated := make(chan result, 1)
	go func() {
		if err := svc.AwaitSpawnReady(ctx); err != nil {
			gated <- result{err: err}
			return
		}
		inst, err := mgr.SpawnDaemon(ctx, "ingress_probe", "ingress_probe", nil)
		gated <- result{inst, err}
	}()

	go func() { _ = serveMarked(srv, lis, serving) }() // the kernel serves
	<-serving.C()

	// The control: an ungated spawn now is refused by the launch gate.
	if _, err := mgr.SpawnDaemon(ctx, "ingress_probe", "control_stream", nil); !errors.Is(err, pyenv.ErrBuilding) {
		t.Fatalf("ungated spawn while the env builds: want pyenv.ErrBuilding, got %v", err)
	}
	select {
	case r := <-gated:
		t.Fatalf("the gated spawn went while the env was building: %+v", r)
	case <-time.After(time.Second):
	}
	if _, live := mgr.DaemonInstanceID("ingress_probe"); live {
		t.Fatal("a daemon is registered while the env builds")
	}

	// The build finishes; nothing restarts the kernel.
	env.finish()
	var r result
	select {
	case r = <-gated:
	case <-ctx.Done():
		t.Fatal("the gated spawn never went once the env was ready")
	}
	if r.err != nil {
		t.Fatalf("gated spawn once the env was ready: %v", r.err)
	}
	inst, live := mgr.DaemonInstanceID("ingress_probe")
	if !live || inst != r.inst {
		t.Fatalf("daemon after the spawn: instance=%q live=%v; want %q live", inst, live, r.inst)
	}
	if n := mgr.DaemonRefCount("ingress_probe"); n != 1 {
		t.Fatalf("daemon ref count %d; want exactly one spawn", n)
	}
	// It stays up on the same instance: no crash-and-restart behind the spawn.
	time.Sleep(2 * time.Second)
	if again, live := mgr.DaemonInstanceID("ingress_probe"); !live || again != inst {
		t.Fatalf("daemon instance changed or died: %q live=%v (was %q)", again, live, inst)
	}
}
