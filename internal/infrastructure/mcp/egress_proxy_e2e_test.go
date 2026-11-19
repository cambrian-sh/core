package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ENG-1545 end to end: an http MCP server reached only through a forward proxy
// (the enterprise case HTTP(S)_PROXY describes) connects, its tools are
// discovered and a call returns — every byte through a real forwarding proxy. The
// endpoint policy still decides first: without the private-network opt-in the
// loopback target is refused before the proxy is asked anything.
func TestConnector_HTTPServerThroughAForwardProxy(t *testing.T) {
	ctx := context.Background()
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "behind-proxy", Version: "1.0.0"}, nil)
	mcpsdk.AddTool(server, &mcpsdk.Tool{Name: "echo", Description: "echo"},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, in echoIn) (*mcpsdk.CallToolResult, echoOut, error) {
			return nil, echoOut{Echo: in.Value}, nil
		})
	target := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil))
	defer target.Close()

	// A forwarding proxy: it relays absolute-URI requests to their target,
	// streaming (FlushInterval -1), and counts them.
	var hits atomic.Int32
	relay := &httputil.ReverseProxy{
		Director:      func(r *http.Request) { r.Host = r.URL.Host },
		FlushInterval: -1,
	}
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !r.URL.IsAbs() {
			http.Error(w, "forward proxy: absolute URI required", http.StatusBadRequest)
			return
		}
		hits.Add(1)
		relay.ServeHTTP(w, r)
	}))
	defer proxySrv.Close()
	proxyURL, _ := url.Parse(proxySrv.URL)
	viaProxy := func(*http.Request) (*url.URL, error) { return proxyURL, nil }

	// Without the opt-in: refused before the proxy.
	closed := NewConnector(CredentialSources{})
	defer closed.Close()
	if tools := closed.ConnectAll(ctx, []ServerConfig{{ID: "behind", Transport: "http", Endpoint: target.URL, proxy: viaProxy}}); len(tools) != 0 {
		t.Fatalf("a loopback target connected without the opt-in: %v", tools)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the proxy was asked for a refused target (%d requests)", n)
	}

	// With the opt-in: through the proxy, tools discovered and a call answered.
	conn := NewConnector(CredentialSources{})
	defer conn.Close()
	tools := conn.ConnectAll(ctx, []ServerConfig{{
		ID: "behind", Transport: "http", Endpoint: target.URL, AllowPrivateNetwork: true, proxy: viaProxy,
		Tools: map[string]ToolPolicy{"echo": {}},
	}})
	found := false
	for _, tl := range tools {
		found = found || tl.Name == "mcp:behind/echo"
	}
	if !found {
		t.Fatalf("echo not discovered through the proxy: %v", tools)
	}
	sess, ok := conn.Session("behind")
	if !ok {
		t.Fatal("no session for the proxied server")
	}
	res, err := sess.CallTool(ctx, &mcpsdk.CallToolParams{Name: "echo", Arguments: map[string]any{"value": "hi"}})
	if err != nil || res.IsError {
		t.Fatalf("call through the proxy: %+v %v", res, err)
	}
	if n := hits.Load(); n < 2 {
		t.Fatalf("proxy relayed %d requests, want the handshake and the call", n)
	}
}
