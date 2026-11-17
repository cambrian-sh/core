//go:build linux

package app

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/metabolism/agentmgr"
)

// ADR-0157 S2 end to end, network by trust tier, through the REAL launch path:
// InstanceManager.GetOrBootInstance → admitLaunch → buildAgentCmd →
// startSandboxed → the app's kernel socket (agentSocketServer.Open →
// listenAgentUnix) → sandbox.StartCmd under bubblewrap → a Python agent that
// uses the SDK's own channel builder (cambrian_agent_sdk.channel.build_channel)
// to dial whatever --substrate-addr the kernel passed.
//
// The "kernel" is a gRPC server serving the TCP plane and the kernel sockets,
// exactly as app.go does (one server, both listeners). The "external host" is
// a TCP listener of this test on a NON-loopback interface: reachable from the
// host's network namespace, unreachable from an empty one. The probe reports
// over the kernel connection itself — a grpc.health.v1 Check whose service
// name carries its findings — so a report that arrives proves the kernel was
// reached.
//
// Asserted:
//   - per_tier, third-party: kernel reached over unix:, external host blocked;
//   - per_tier, first-party: kernel reached over TCP, external host reached;
//   - kernel_only_all, first-party: kernel reached over unix:, external blocked.
//
// It needs bubblewrap and a Python with the SDK:
//
//	KERNEL_ONLY_E2E_PY  the interpreter (a venv with cambrian_agent_sdk)
//
// Run as a non-root uid the third-party tier lacks low_privilege (no lower uid
// to drop to), so it runs with on_unavailable degrade and the network control
// is asserted enforced; run as root every third-party control is enforced and
// the agent is admitted under the default refuse.
func TestE2E_NetworkByTrustTier(t *testing.T) {
	py := os.Getenv("KERNEL_ONLY_E2E_PY")
	if py == "" {
		t.Skip("set KERNEL_ONLY_E2E_PY to a Python with the agent SDK to run the kernel-only network e2e")
	}
	if out, err := exec.Command("bwrap", "--ro-bind", "/", "/", "--unshare-net", "--unshare-user", "--", "/bin/true").CombinedOutput(); err != nil {
		t.Skipf("bubblewrap unusable here: %v %s", err, out)
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

	// The kernel: one gRPC server on TCP and on the kernel sockets.
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

	root, err := os.MkdirTemp("/tmp", "s2e")
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(root, 0o755)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	code := filepath.Join(root, "code")
	kernelDir := filepath.Join(root, "kernel")
	for _, d := range []string{code, filepath.Join(kernelDir, "configs")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(code, "probe.py"), []byte(networkProbePy), 0o644); err != nil {
		t.Fatal(err)
	}
	asRoot := os.Geteuid() == 0

	for _, tc := range []struct {
		name          string
		kernelOnlyAll bool
		tier          string
		wantUnix      bool
		wantExternal  string
	}{
		{"per_tier/third-party", false, "third-party", true, "blocked"},
		{"per_tier/first-party", false, "first-party", false, "reached"},
		{"kernel_only_all/first-party", true, "first-party", true, "blocked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sb := agentmgr.NewAgentSandbox(agentmgr.AgentSandboxOptions{
				WorkRoot:         filepath.Join(root, "w"),
				KernelDir:        kernelDir,
				KernelStatePaths: []string{filepath.Join(kernelDir, "configs")},
				// Non-root: no lower uid exists, so third-party degrades over
				// low_privilege ONLY; as root it is admitted under refuse.
				ThirdPartyRefuse: asRoot,
				KernelOnlyAll:    tc.kernelOnlyAll,
			})
			im := agentmgr.NewInstanceManager(py, tcp.Addr().String())
			im.SetAgentContainment(agentmgr.AgentContainment{Sandbox: sb, ThirdPartyDegrade: !asRoot})
			im.SetAgentTransport(agentmgr.AgentTransport{KernelSocket: sockets.Open})
			im.SetAgentBootTimeout(60 * time.Second)
			id := "probe_" + strings.NewReplacer("/", "_", "-", "_").Replace(tc.name)
			def := &domain.AgentDefinition{ID: id, Runtime: domain.RuntimePython, ExecPath: "probe.py", Dir: code,
				TrustTier: tc.tier, LaunchArgs: []string{"--external", ext.Addr().String(), "--probe-id", id}}
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			inst, err := im.GetOrBootInstance(ctx, def, "")
			if err != nil {
				t.Fatalf("boot: %v", err)
			}
			defer im.EvictInstance(inst.ID)
			got := rec.waitFor(ctx, "probe="+id+";")
			if got == "" {
				t.Fatalf("no report from %s over the kernel connection (kernel unreachable?); seen %v", id, rec.all())
			}
			t.Logf("report: %s", got)
			if strings.Contains(got, "addr=unix:") != tc.wantUnix {
				t.Errorf("%s dialled the kernel at the wrong address: %s", id, got)
			}
			if !strings.Contains(got, "external="+tc.wantExternal) {
				t.Errorf("%s: want external=%s, got %s", id, tc.wantExternal, got)
			}
		})
	}
}

// networkProbePy is the probe agent: it serves its --socket (so the kernel's
// boot wait sees it), tries the external host, and reports both over the
// kernel connection the SDK builds from --substrate-addr.
const networkProbePy = `
import socket, sys, time
from concurrent import futures
import grpc
from cambrian_agent_sdk.channel import build_channel

def arg(name):
    a = sys.argv
    return a[a.index(name) + 1] if name in a else ""

sock, addr, ext, pid = arg("--socket"), arg("--substrate-addr"), arg("--external"), arg("--probe-id")
server = grpc.server(futures.ThreadPoolExecutor(max_workers=1))
server.add_insecure_port("unix:" + sock)
server.start()

host, port = ext.rsplit(":", 1)
try:
    socket.create_connection((host, int(port)), timeout=3).close()
    external = "reached"
except OSError as e:
    external = "blocked(" + type(e).__name__ + ")"

report = "probe=%s;addr=%s;external=%s" % (pid, addr, external)
raw = report.encode()
def varint(n):
    out = b""
    while True:
        b, n = n & 0x7F, n >> 7
        out += bytes([b | (0x80 if n else 0)])
        if not n:
            return out
req = b"\x0a" + varint(len(raw)) + raw  # HealthCheckRequest{service: report}
check = build_channel(addr).unary_unary("/grpc.health.v1.Health/Check",
    request_serializer=lambda b: b, response_deserializer=lambda b: b)
for _ in range(50):
    try:
        check(req, timeout=3)
        break
    except grpc.RpcError as e:
        print("kernel not reached yet:", e.code(), flush=True)
        time.sleep(0.2)
time.sleep(600)
`

// recordingHealth records the service names Health/Check was asked about.
type recordingHealth struct {
	healthpb.UnimplementedHealthServer
	mu   sync.Mutex
	seen []string
}

func (h *recordingHealth) Check(_ context.Context, r *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	h.mu.Lock()
	h.seen = append(h.seen, r.GetService())
	h.mu.Unlock()
	return &healthpb.HealthCheckResponse{Status: healthpb.HealthCheckResponse_SERVING}, nil
}

func (h *recordingHealth) all() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.seen...)
}

func (h *recordingHealth) waitFor(ctx context.Context, prefix string) string {
	for {
		for _, s := range h.all() {
			if strings.HasPrefix(s, prefix) {
				return s
			}
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func nonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && n.IP.To4() != nil {
			return n.IP.String()
		}
	}
	t.Skip("no non-loopback IPv4 interface to stand in for an external host")
	return ""
}
