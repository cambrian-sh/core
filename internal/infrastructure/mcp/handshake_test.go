package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ENG-1528: the MCP connect handshake is bounded, and only the handshake.

// silentServer accepts every request and never answers it — the shape of an
// http/sse server that takes the connection and then hangs. Its handlers
// return when the client gives up (request ctx done) or the test ends.
func silentServer(t *testing.T) string {
	t.Helper()
	stop := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	t.Cleanup(func() { close(stop); ts.Close() })
	return ts.URL
}

// (a) A server that never answers initialize fails the boot ConnectAll
// within the bound instead of hanging the kernel boot — on both HTTP
// transports — and its health says why.
func TestConnectAll_SilentServerFailsWithinTheHandshakeBound(t *testing.T) {
	for _, transport := range []string{"http", "sse"} {
		t.Run(transport, func(t *testing.T) {
			url := silentServer(t)
			conn := NewConnector(CredentialSources{}, WithTimeouts(ConnectorTimeouts{Handshake: 300 * time.Millisecond}))
			defer conn.Close()

			done := make(chan int, 1)
			start := time.Now()
			go func() {
				done <- len(conn.ConnectAll(context.Background(), []ServerConfig{{
					ID: "silent", Transport: transport, Endpoint: url, AllowPrivateNetwork: true,
				}}))
			}()
			select {
			case n := <-done:
				if n != 0 {
					t.Fatalf("a silent server registered %d tools", n)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("ConnectAll hung on a server that never answers initialize")
			}
			// The bound, plus go-sdk's own tail: a cancelled call first sends
			// notifications/cancelled under the SDK's 5 s notify timeout, and
			// on Streamable HTTP the silent server swallows that POST too
			// (server/discover, then the initialize fallback: up to 2 x 5 s).
			if el := time.Since(start); el > 300*time.Millisecond+12*time.Second {
				t.Fatalf("ConnectAll took %s; the handshake bound is 300ms", el)
			}
			h, ok := conn.Health("silent")
			if !ok || h.State != HealthError || !strings.Contains(h.LastError, "handshake") {
				t.Fatalf("health should report the handshake timeout, got %+v", h)
			}
			if _, live := conn.Session("silent"); live {
				t.Fatal("a timed-out server must not leave a session behind")
			}
		})
	}
}

// The bound is a sentinel callers can tell apart from a refused connection.
func TestConnectSession_TimeoutIsDistinguishable(t *testing.T) {
	conn := NewConnector(CredentialSources{}, WithTimeouts(ConnectorTimeouts{Handshake: 200 * time.Millisecond}))
	defer conn.Close()
	s := ServerConfig{ID: "silent", Transport: "sse", Endpoint: silentServer(t), AllowPrivateNetwork: true}
	_, _, err := conn.connect(context.Background(), s)
	if !errors.Is(err, ErrHandshakeTimeout) {
		t.Fatalf("want ErrHandshakeTimeout, got %v", err)
	}
}

// sseEchoServer is a legacy HTTP+SSE MCP server (2024-11-05 transport) with
// one read tool. It records the Authorization header of every request (the
// legacy transport gives a tool handler no request headers).
func sseEchoServer(t *testing.T) (string, func() []string) {
	t.Helper()
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "sse", Version: "1.0.0"}, nil)
	mcpsdk.AddTool(server, &mcpsdk.Tool{Name: "whoami", Description: "answer"},
		func(context.Context, *mcpsdk.CallToolRequest, struct{}) (*mcpsdk.CallToolResult, map[string]string, error) {
			return nil, map[string]string{"answer": "pong"}, nil
		})
	h := mcpsdk.NewSSEHandler(func(*http.Request) *mcpsdk.Server { return server }, nil)
	var mu sync.Mutex
	var auths []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), auths...)
	}
}

// (b) A per-user SSE session still answers tool calls after its connect
// returned: the SSE GET stream carries every response, so a stream bound to
// the dial's own deadline context died the moment dialUser returned.
func TestPerUser_SSE_SessionOutlivesItsConnect(t *testing.T) {
	url, auths := sseEchoServer(t)
	creds := newFakeUserCreds()
	creds.set("crm", "alice", "tok-alice-7777")
	// The first call dials; the later ones reuse the pooled session after its
	// connect has long returned (and after the handshake timer would have
	// fired, had it not been stopped).
	conn := NewConnector(CredentialSources{User: creds}, WithTimeouts(ConnectorTimeouts{Handshake: 300 * time.Millisecond}))
	defer conn.Close()
	cfg := perUserHTTP("crm", url, "bearer", "")
	cfg.Transport = "sse"
	conn.rememberConfig(cfg)
	h := &Handler{Sessions: conn}

	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		out, err := callAs(ctx, h, "alice", "mcp:crm/whoami")
		cancel()
		if err != nil || !strings.Contains(out, "pong") {
			t.Fatalf("call %d on the per-user SSE session: %q %v", i, out, err)
		}
		time.Sleep(400 * time.Millisecond)
	}
	if got := conn.UserInstances(); len(got) != 1 {
		t.Fatalf("want the one pooled session reused, got %v", got)
	}
	for _, a := range auths() {
		if a != "Bearer tok-alice-7777" {
			t.Fatalf("a request carried %q, not alice's token", a)
		}
	}
}

// The shared SSE session likewise keeps working past the handshake bound.
func TestShared_SSE_SessionOutlivesTheHandshakeBound(t *testing.T) {
	url, _ := sseEchoServer(t)
	conn := NewConnector(CredentialSources{}, WithTimeouts(ConnectorTimeouts{Handshake: 300 * time.Millisecond}))
	defer conn.Close()
	tools := conn.ConnectAll(context.Background(), []ServerConfig{{
		ID: "crm", Transport: "sse", Endpoint: url, AllowPrivateNetwork: true,
		DefaultPolicy: &ToolPolicy{Effects: []string{"read"}},
	}})
	if len(tools) != 1 {
		t.Fatalf("want the one tool discovered, got %d", len(tools))
	}
	time.Sleep(500 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := callAs(ctx, &Handler{Sessions: conn}, "", "mcp:crm/whoami"); err != nil {
		t.Fatalf("shared SSE session died after the handshake bound: %v", err)
	}
}

// ENG-2003: the bounds are config. Unset keeps the built-in defaults; a
// configured value replaces each independently.
func TestWithTimeouts_DefaultsAndOverrides(t *testing.T) {
	def := NewConnector(CredentialSources{})
	defer def.Close()
	if got := def.handshakeTimeout(); got != 60*time.Second {
		t.Fatalf("default handshake bound = %s, want 60s", got)
	}
	if got := def.pingTimeout(); got != 10*time.Second {
		t.Fatalf("default ping bound = %s, want 10s", got)
	}
	zero := NewConnector(CredentialSources{}, WithTimeouts(ConnectorTimeouts{}))
	defer zero.Close()
	if zero.handshakeTimeout() != 60*time.Second || zero.pingTimeout() != 10*time.Second {
		t.Fatal("a zero ConnectorTimeouts must keep the defaults")
	}
	set := NewConnector(CredentialSources{}, WithTimeouts(ConnectorTimeouts{Handshake: 5 * time.Second, Ping: 2 * time.Second}))
	defer set.Close()
	if set.handshakeTimeout() != 5*time.Second || set.pingTimeout() != 2*time.Second {
		t.Fatalf("configured bounds not applied: %s / %s", set.handshakeTimeout(), set.pingTimeout())
	}
}
