package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// Config-driven: the Connector dials a server defined by config over Streamable
// HTTP, injects the static bearer credential, discovers its tools as
// mcp:<id>/<tool>, and serves sessions so a call routes end-to-end through the
// ToolExecutor (ADR-0043 D2/D3/D9). Hermetic — a real in-process MCP HTTP server.
func TestConnector_ConfigDrivenConnectDiscoverAuth(t *testing.T) {
	ctx := context.Background()

	// In-process MCP server over Streamable HTTP, capturing the auth header.
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "srv", Version: "1.0.0"}, nil)
	mcpsdk.AddTool(server, &mcpsdk.Tool{Name: "echo", Description: "echo"},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, in echoIn) (*mcpsdk.CallToolResult, echoOut, error) {
			return nil, echoOut{Echo: in.Value}, nil
		})
	sdkHandler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil)
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := r.Header.Get("Authorization"); a != "" {
			gotAuth = a
		}
		sdkHandler.ServeHTTP(w, r)
	}))
	defer ts.Close()

	t.Setenv("MCP_TEST_TOKEN", "secret-xyz")
	conn := NewConnector()
	defer conn.Close()

	tools := conn.ConnectAll(ctx, []ServerConfig{{
		ID: "srv", Transport: "http", Endpoint: ts.URL,
		AuthType: "bearer", AuthTokenEnv: "MCP_TEST_TOKEN",
	}})

	// Discovery registered the server's tool under the namespaced identity.
	found := false
	for _, tl := range tools {
		if tl.Name == "mcp:srv/echo" {
			found = true
		}
	}
	if !found {
		t.Fatalf("discovery should register mcp:srv/echo, got %d tools", len(tools))
	}

	// End-to-end call through the ToolExecutor, using the connector as the session source.
	reg := domain.NewInMemoryToolRegistry()
	for _, tl := range tools {
		reg.Register(tl)
	}
	grants := domain.NewInMemoryGrantsStore()
	grants.Set("a", []domain.ToolGrant{{Tool: "mcp:srv/echo"}})
	exec := &domain.ToolExecutor{
		Registry:        reg,
		Grants:          grants,
		MCPHandler:      &Handler{Sessions: conn},
		InlineThreshold: 1 << 20,
	}
	resp := exec.Execute(ctx, domain.ToolCallRequest{
		AgentID: "a", ToolName: "mcp:srv/echo", ArgsJSON: []byte(`{"value":"hey"}`),
	})
	if resp.Denied || resp.Error != "" {
		t.Fatalf("config-driven MCP call failed: denied=%v err=%q", resp.Denied, resp.Error)
	}
	if !strings.Contains(string(resp.ResultJSON), "hey") {
		t.Errorf("echo result wrong: %s", resp.ResultJSON)
	}

	// The static bearer credential reached the server.
	if gotAuth != "Bearer secret-xyz" {
		t.Errorf("server should receive the bearer token, got %q", gotAuth)
	}
}

// The legacy HTTP+SSE transport (transport: "sse") connects, discovers, and calls
// end-to-end — the shape self-hosted Firecrawl's /sse endpoint serves (ADR-0043 D2).
func TestConnector_SSETransport(t *testing.T) {
	ctx := context.Background()
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "srv", Version: "1.0.0"}, nil)
	mcpsdk.AddTool(server, &mcpsdk.Tool{Name: "echo", Description: "echo"},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, in echoIn) (*mcpsdk.CallToolResult, echoOut, error) {
			return nil, echoOut{Echo: in.Value}, nil
		})
	ts := httptest.NewServer(mcpsdk.NewSSEHandler(func(*http.Request) *mcpsdk.Server { return server }, nil))
	defer ts.Close()

	conn := NewConnector()
	defer conn.Close()
	tools := conn.ConnectAll(ctx, []ServerConfig{{ID: "srv", Transport: "sse", Endpoint: ts.URL}})

	found := false
	for _, tl := range tools {
		if tl.Name == "mcp:srv/echo" {
			found = true
		}
	}
	if !found {
		t.Fatalf("sse discovery should register mcp:srv/echo, got %d tools", len(tools))
	}

	h := &Handler{Sessions: conn}
	out, err := h.Execute(ctx, domain.ToolCall{ToolName: "mcp:srv/echo", ArgsJSON: []byte(`{"value":"via-sse"}`)})
	if err != nil {
		t.Fatalf("sse call failed: %v", err)
	}
	if !strings.Contains(string(out), "via-sse") {
		t.Errorf("sse echo result wrong: %s", out)
	}
}

// Operator per-tool policy (dangerous-flag + data-egress kinds) is applied to the
// discovered SystemTool — not the server's own advertised metadata (ADR-0043 D4/D9).
func TestConnector_AppliesToolPolicyToDiscoveredTools(t *testing.T) {
	ctx := context.Background()
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "srv", Version: "1.0.0"}, nil)
	mcpsdk.AddTool(server, &mcpsdk.Tool{Name: "scrape", Description: "scrape"},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, in echoIn) (*mcpsdk.CallToolResult, echoOut, error) {
			return nil, echoOut{Echo: in.Value}, nil
		})
	ts := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil))
	defer ts.Close()

	conn := NewConnector()
	defer conn.Close()
	tools := conn.ConnectAll(ctx, []ServerConfig{{
		ID: "srv", Transport: "http", Endpoint: ts.URL,
		Tools: map[string]ToolPolicy{"scrape": {Dangerous: true, DataWriteKinds: []string{"pii"}}},
	}})

	var got *domain.SystemTool
	for i := range tools {
		if tools[i].Name == "mcp:srv/scrape" {
			got = &tools[i]
		}
	}
	if got == nil {
		t.Fatal("scrape not discovered")
	}
	if !got.Dangerous {
		t.Error("operator policy dangerous=true must apply to the discovered tool")
	}
	if len(got.DataWriteKinds) != 1 || got.DataWriteKinds[0] != "pii" {
		t.Errorf("DataWriteKinds = %v, want [pii]", got.DataWriteKinds)
	}
}

// A server that fails to connect is skipped gracefully — it does not fail the
// others or the kernel (ADR-0043 D8).
func TestConnector_UnreachableServerSkippedGracefully(t *testing.T) {
	ctx := context.Background()
	conn := NewConnector()
	defer conn.Close()

	tools := conn.ConnectAll(ctx, []ServerConfig{
		{ID: "dead", Transport: "http", Endpoint: "http://127.0.0.1:1/nope"},
	})
	if len(tools) != 0 {
		t.Errorf("an unreachable server should contribute no tools, got %d", len(tools))
	}
	if _, ok := conn.Session("dead"); ok {
		t.Error("an unreachable server should have no session")
	}
}
