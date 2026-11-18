package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/mcp"
)

// ENG-2003: mcp.handshake_timeout_ms reaches the connector the kernel boots
// with. A server that accepts the connection and never answers initialize
// fails the boot ConnectAll at the CONFIGURED bound (300 ms here), not at the
// 60 s default — through the same mcpConnectorOptions app.go constructs with.
func TestMCPConnector_ConfiguredHandshakeBoundReachesTheBootConnect(t *testing.T) {
	stop := make(chan struct{})
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	t.Cleanup(func() { close(stop); silent.Close() })

	cfg := &config.Config{MCP: config.MCPConfig{HandshakeTimeoutMs: 300}}
	conn := mcp.NewConnector(mcpCredentialSources(cfg, nil, nil), mcpConnectorOptions(cfg, nil)...)
	defer conn.Close()

	start := time.Now()
	done := make(chan int, 1)
	go func() {
		done <- len(conn.ConnectAll(context.Background(), []mcp.ServerConfig{{
			ID: "silent", Transport: "sse", Endpoint: silent.URL, AllowPrivateNetwork: true,
		}}))
	}()
	select {
	case n := <-done:
		if n != 0 {
			t.Fatalf("a silent server registered %d tools", n)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("ConnectAll hung: the configured handshake bound did not reach the connector")
	}
	// A silent legacy-SSE server fails at the limit (handshake.go); allow the
	// SDK's 5 s cancellation-notify tail, far below the 60 s default.
	if el := time.Since(start); el > 300*time.Millisecond+7*time.Second {
		t.Fatalf("ConnectAll took %s with a 300ms configured bound", el)
	}
	h, ok := conn.Health("silent")
	if !ok || h.State != mcp.HealthError || !strings.Contains(h.LastError, "after 300ms") {
		t.Fatalf("health should report the 300ms handshake timeout, got %+v", h)
	}
}
