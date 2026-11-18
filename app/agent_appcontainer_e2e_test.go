//go:build windows

package app

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/metabolism/agentmgr"
)

// ADR-0157 S5 end to end on Windows, through the REAL launch path:
// GetOrBootInstance → admitLaunch → buildAgentCmd → startSandboxed → the app's
// kernel socket (agentSocketServer.Open → listenAgentUnix, AF_UNIX on Windows)
// → sandbox.StartCmd → the AppContainer runner → a Python agent using the
// SDK's own channel builder to dial whatever --substrate-addr it was given.
//
// The "kernel" is one gRPC server on TCP and on the kernel sockets, as app.go
// runs it. The probe reports over that connection, so a report that arrives
// proves the kernel was reached.
//
// Asserted, per case:
//   - every tier dials the kernel over unix: (an AppContainer has no loopback);
//   - the kernel's config.db is unreadable from the agent;
//   - this host's own non-loopback address is unreachable;
//   - third-party and kernel_only_all first-party: a public host is
//     unreachable; per_tier first-party: reachable when this test process can
//     reach it (the internetClient capability).
//
// It runs on the kernel-managed sandbox Python env (ADR-0157 D7 P2): the
// kernel owns that tree, so it can always grant it, and every case refuses on
// any missing control — a fallback to the low-integrity token (no AppContainer)
// fails the boot. The spawn time (grant + boot) is logged per case: the D7
// grant-cost measurement.
//
//	KERNEL_ONLY_E2E_PY  the managed env's interpreter (python-env status prints it)
//	KERNEL_ONLY_E2E_INTERNET  optional public host:port (default 1.1.1.1:443)
func TestE2E_AppContainerNetworkAndStateByTier(t *testing.T) {
	py := os.Getenv("KERNEL_ONLY_E2E_PY")
	if py == "" {
		t.Skip("set KERNEL_ONLY_E2E_PY to the kernel-managed env's interpreter (python-env status) to run the AppContainer e2e")
	}
	internet := os.Getenv("KERNEL_ONLY_E2E_INTERNET")
	if internet == "" {
		internet = "1.1.1.1:443"
	}
	hostOnline := false
	if c, err := net.DialTimeout("tcp", internet, 3*time.Second); err == nil {
		_ = c.Close()
		hostOnline = true
	}
	extIP := nonLoopbackIPv4(t)
	ext, err := net.Listen("tcp", net.JoinHostPort(extIP, "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer ext.Close()
	go func() {
		for {
			c, err := ext.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

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
	defer func() {
		srv.Stop()
		sockets.stop()
	}()

	root, err := os.MkdirTemp("", "s5e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	code := filepath.Join(root, "code")
	kernelDir := filepath.Join(root, "kernel")
	configDB := filepath.Join(kernelDir, "configs", "config.db")
	for _, d := range []string{code, filepath.Dir(configDB)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(configDB, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(code, "probe.py"), []byte(networkProbePy), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name          string
		kernelOnlyAll bool
		tier          string
		wantInternet  bool
	}{
		{"per_tier/third-party", false, "third-party", false},
		{"per_tier/first-party", false, "first-party", true},
		{"kernel_only_all/first-party", true, "first-party", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sb := agentmgr.NewAgentSandbox(agentmgr.AgentSandboxOptions{
				WorkRoot:         filepath.Join(root, "w"),
				KernelDir:        kernelDir,
				KernelStatePaths: []string{filepath.Join(kernelDir, "configs")},
				ThirdPartyRefuse: true, // every third-party control must be enforced
				FirstPartyRefuse: true,
				KernelOnlyAll:    tc.kernelOnlyAll,
			})
			im := instanceManagerWithPython(t, agentmgr.NewInstanceManager(tcp.Addr().String()), py)
			im.SetAgentContainment(agentmgr.AgentContainment{Sandbox: sb})
			im.SetAgentTransport(agentmgr.AgentTransport{KernelSocket: sockets.Open})
			im.SetAgentBootTimeout(60 * time.Second)
			id := "probe_" + strings.NewReplacer("/", "_", "-", "_").Replace(tc.name)
			def := &domain.AgentDefinition{ID: id, Runtime: domain.RuntimePython, ExecPath: "probe.py", Dir: code,
				TrustTier: tc.tier, LaunchArgs: []string{"--external", ext.Addr().String(), "--probe-id", id,
					"--internet", internet, "--secret", configDB}}
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			start := time.Now()
			inst, err := im.GetOrBootInstance(ctx, def, "")
			if err != nil {
				t.Fatalf("boot (refuse on any missing control): %v", err)
			}
			t.Logf("spawn (AppContainer grant + boot to socket): %s", time.Since(start).Round(time.Millisecond))
			defer im.EvictInstance(inst.ID)
			got := rec.waitFor(ctx, "probe="+id+";")
			if got == "" {
				t.Fatalf("no report from %s over the kernel connection; seen %v", id, rec.all())
			}
			t.Logf("report: %s", got)
			if !strings.Contains(got, "addr=unix:") {
				t.Errorf("%s did not dial the kernel socket: %s", id, got)
			}
			if !strings.Contains(got, "secret=denied") {
				t.Errorf("%s read the kernel's config.db: %s", id, got)
			}
			if !strings.Contains(got, "external=blocked") {
				t.Errorf("%s reached this host's own address: %s", id, got)
			}
			switch {
			case !tc.wantInternet && !strings.Contains(got, "internet=blocked"):
				t.Errorf("%s reached the internet: %s", id, got)
			case tc.wantInternet && hostOnline && !strings.Contains(got, "internet=reached"):
				t.Errorf("%s (normal network) could not reach %s: %s", id, internet, got)
			case tc.wantInternet && !hostOnline:
				t.Logf("this host is offline (%s unreachable): first-party internet not asserted", internet)
			}
		})
	}
}
