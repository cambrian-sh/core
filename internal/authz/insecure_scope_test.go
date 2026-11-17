package authz

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0142 K-2: the insecure_localhost waiver is scoped to the host it names. A
// loopback (or in-process) caller keeps the header-is-the-principal behaviour the
// benchmark harness relies on; a caller from a routable address must present the
// per-agent credential even with the flag set.
func TestInsecureLocalhost_WaiverIsLoopbackOnly(t *testing.T) {
	v, err := NewAgentCredentialVerifier(func(string) (string, bool) { return "", false }, NewInstanceTokens(),
		func(context.Context, string) bool { return false }, true)
	if err != nil {
		t.Fatal(err)
	}
	const method = agentServicePrefix + "Execute"
	call := func(addr string) (context.Context, error) {
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-agent-id", "bench-agent"))
		if addr != "" {
			tcp, err := net.ResolveTCPAddr("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			ctx = peer.NewContext(ctx, &peer.Peer{Addr: tcp})
		}
		return v.Authenticate(ctx, method, nil)
	}
	for _, addr := range []string{"", "127.0.0.1:5000", "[::1]:5000"} {
		ctx, err := call(addr)
		if err != nil {
			t.Fatalf("loopback %q: %v", addr, err)
		}
		if p := domain.PrincipalFromContext(ctx); p != domain.AgentPrincipal("bench-agent") {
			t.Fatalf("loopback %q: principal %v", addr, p)
		}
	}
	for _, addr := range []string{"10.1.2.3:5000", "192.168.0.7:41000"} {
		if _, err := call(addr); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("routable %q must need a credential; got %v", addr, err)
		}
	}
}
