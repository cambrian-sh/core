package app

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// The network probe agent and the recording "kernel" shared by the ADR-0157
// network e2e tests: Linux bubblewrap (agent_kernel_only_e2e_test.go, S2) and
// the Windows AppContainer (agent_appcontainer_e2e_test.go, S5).

// networkProbePy is the probe agent: it serves its --socket (so the kernel's
// boot wait sees it), tries the external host (and, when given, a public
// --internet host and a --secret file), and reports over the kernel
// connection the SDK builds from --substrate-addr.
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

# Optional (the Windows AppContainer e2e): a public host, and a kernel file the
# agent must not be able to read.
extra = ""
inet = arg("--internet")
if inet:
    h, p = inet.rsplit(":", 1)
    try:
        socket.create_connection((h, int(p)), timeout=3).close()
        extra += ";internet=reached"
    except OSError as e:
        extra += ";internet=blocked(" + type(e).__name__ + ")"
secret = arg("--secret")
if secret:
    try:
        open(secret, "rb").read()
        extra += ";secret=read"
    except OSError as e:
        extra += ";secret=denied(" + type(e).__name__ + ")"

report = "probe=%s;addr=%s;external=%s" % (pid, addr, external) + extra
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
