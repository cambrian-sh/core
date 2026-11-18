package app

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// mcpCfg builds a config whose MCP endpoint is enabled on the given bind.
func mcpCfg(bind string, insecure bool, port int) *config.Config {
	c := cfgWith(bind, "", "", insecure)
	c.Server.MCP = config.MCPEndpointConfig{
		Enabled:                true,
		Port:                   port,
		Clients:                []string{"ci-bot"},
		MaxConcurrentPerClient: 2,
	}
	return c
}

// THE gate this slice must pass. The MCP endpoint is the one surface strangers
// are meant to reach, so it inherits SEC-03 rather than opening its own hole: a
// routable bind with no certificate and no explicit opt-in must REFUSE, and it
// must refuse before the port is open.
//
// Testable without binding a routable interface because the transport decision
// is made first — which is itself the property (a refusal discovered after the
// listener exists would be a port that briefly served).
func TestStartMCPEndpoint_RefusesPlaintextOnRoutableBind(t *testing.T) {
	for _, bind := range []string{"0.0.0.0", "192.168.1.10", "::", "example.internal"} {
		srv, err := startMCPEndpoint(mcpCfg(bind, false, 0), nil, nil, nil, nil)
		if err == nil {
			if srv != nil {
				_ = srv.Close()
			}
			t.Fatalf("bind %q: the MCP endpoint bound plaintext on a routable address", bind)
		}
		if !strings.Contains(err.Error(), "PLAINTEXT") {
			t.Errorf("bind %q: error does not name the problem: %v", bind, err)
		}
		if srv != nil {
			t.Errorf("bind %q: a refused endpoint still returned a server", bind)
		}
	}
}

// Default disabled: nothing is bound and nothing is validated. An operator who
// never asked for the endpoint gets no listener and no error.
func TestStartMCPEndpoint_DisabledBindsNothing(t *testing.T) {
	srv, err := startMCPEndpoint(cfgWith("0.0.0.0", "", "", false), nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("a disabled endpoint must not fail the boot: %v", err)
	}
	if srv != nil {
		_ = srv.Close()
		t.Fatal("a disabled endpoint started a server")
	}
}

// An endpoint that could authenticate nobody must not start. The failure is at
// BOOT rather than per-request, because a running endpoint that 401s everything
// is indistinguishable from a wrong token on the client side.
func TestStartMCPEndpoint_RefusesAnUnservableSurface(t *testing.T) {
	cfg := mcpCfg("127.0.0.1", false, 0)
	cfg.Server.MCP.Clients = nil
	srv, err := startMCPEndpoint(cfg, nil, nil, nil, nil)
	if err == nil {
		if srv != nil {
			_ = srv.Close()
		}
		t.Fatal("an endpoint with no configured client started")
	}
}

// The happy path on the shipped profile: loopback, no certificate, zero
// published tools. Port 0 lets the OS choose, so the test binds nothing a
// developer's machine might already hold.
func TestStartMCPEndpoint_LoopbackServesAndDrains(t *testing.T) {
	srv, err := startMCPEndpoint(mcpCfg("127.0.0.1", false, 0), domain.PublishedToolSurface{}, nil, nil, nil)
	if err != nil {
		t.Fatalf("loopback plaintext refused: %v", err)
	}
	if srv == nil {
		t.Fatal("an enabled endpoint returned no server")
	}
	if err := srv.Close(); err != nil && err != http.ErrServerClosed {
		t.Errorf("close: %v", err)
	}
}

// Boot order (ADR-0126 D3), pinned against the refactor that would break it
// silently. ConnectAll is synchronous and takes 40-45 s; an endpoint started
// after it would be unreachable for the whole of boot, which is exactly the
// `claude mcp add` timeout the ordering exists to prevent — and the symptom
// (a kernel that looks up) gives no hint of the cause.
//
// Since ENG-1123 the two calls live in separate boot phases (boot_tools.go), so
// the order is pinned in two steps: each call is in its phase, and
// bootstrapKernel (boot.go) runs the endpoint phase before the connector phase.
func TestBootOrder_MCPEndpointStartsBeforeConnectAll(t *testing.T) {
	tools := readSource(t, "boot_tools.go")
	inPhase := func(phase, call string) {
		t.Helper()
		body := funcBody(t, tools, "func (b *kernelBuild) "+phase+"(")
		if !strings.Contains(body, call) {
			t.Fatalf("%s is no longer called in %s; if it moved or was renamed, keep the "+
				"ordering (ADR-0126 D3) and update this test", call, phase)
		}
	}
	inPhase("buildMCPEndpoint", "startMCPEndpoint(")
	inPhase("buildMCPConnector", "mcpConnector.ConnectAll(ctx, enabledMCP)")

	boot := funcBody(t, readSource(t, "boot.go"), "func bootstrapKernel(")
	endpoint := strings.Index(boot, "b.buildMCPEndpoint()")
	connector := strings.Index(boot, "b.buildMCPConnector()")
	if endpoint < 0 || connector < 0 {
		t.Fatalf("phase calls not found in bootstrapKernel (endpoint=%d connector=%d)", endpoint, connector)
	}
	if endpoint > connector {
		t.Fatal("the MCP endpoint now starts AFTER Connector.ConnectAll; the endpoint would be " +
			"unreachable for the 40-45 s of MCP client init (ADR-0126 D3)")
	}
}

func readSource(t *testing.T, name string) string {
	t.Helper()
	src, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(src)
}

// funcBody returns src from the line starting with decl to the next top-level
// closing brace.
func funcBody(t *testing.T, src, decl string) string {
	t.Helper()
	start := strings.Index(src, decl)
	if start < 0 {
		t.Fatalf("%q not found", decl)
	}
	end := strings.Index(src[start:], "\n}\n")
	if end < 0 {
		t.Fatalf("could not delimit %q", decl)
	}
	return src[start : start+end]
}

// The D4 identity hop reaches the endpoint (ADR-0126 E6). Pinned in source
// because the failure is invisible: dropping the argument leaves a premium
// deployment silently on the phase-1 binding — every registered client still
// authenticates, every binding an operator authored is simply never consulted,
// and blocking a client has no effect on the surface it arrives through.
func TestWiring_MCPEndpointReceivesTheIdentityResolver(t *testing.T) {
	call := readSource(t, "boot_tools.go") // the buildMCPEndpoint phase (ENG-1123)
	start := strings.Index(call, "startMCPEndpoint(")
	if start < 0 {
		t.Fatal("startMCPEndpoint call site not found")
	}
	end := strings.Index(call[start:], "\n\tif err != nil")
	if end < 0 {
		t.Fatal("could not delimit the startMCPEndpoint call")
	}
	if !strings.Contains(call[start:start+end], "opts.IdentityResolver") {
		t.Fatal("startMCPEndpoint is no longer handed opts.IdentityResolver; the mcp surface " +
			"would ignore every identity binding an operator authored (ADR-0126 D4)")
	}
}
