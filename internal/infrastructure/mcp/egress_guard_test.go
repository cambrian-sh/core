package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cambrian-sh/core/pkg/egress"
)

// ADR-0153 (owner decision 2026-11-14, ENG-1419): MCP http/sse transports dial
// through the guarded egress client. A loopback server is unreachable without
// the per-endpoint allow_private_network opt-in and reachable with it; the
// static credential still reaches the server.
func TestHTTPClient_GuardedEgressNeedsTheOptIn(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
	}))
	defer srv.Close()
	t.Setenv("MCP_EGRESS_TEST_TOKEN", "tok-1")

	s := ServerConfig{ID: "loop", Transport: "http", Endpoint: srv.URL, AuthType: "bearer", AuthTokenEnv: "MCP_EGRESS_TEST_TOKEN"}
	hc, _ := s.httpClient(context.Background())
	if _, err := hc.Get(srv.URL); !errors.Is(err, egress.ErrBlockedAddr) {
		t.Fatalf("loopback MCP server reached without the opt-in: err = %v", err)
	}

	s.AllowPrivateNetwork = true
	hc, tok := s.httpClient(context.Background())
	resp, err := hc.Get(srv.URL)
	if err != nil {
		t.Fatalf("opted-in loopback server: %v", err)
	}
	resp.Body.Close()
	if tok != "tok-1" || gotAuth != "Bearer tok-1" {
		t.Fatalf("credential not delivered: tok=%q header=%q", tok, gotAuth)
	}
	if hc.Timeout != 0 {
		t.Fatalf("client timeout %v would cut the SSE stream", hc.Timeout)
	}
}

// The per-user session client is guarded the same way.
func TestPerUserHTTPClient_IsGuarded(t *testing.T) {
	s := ServerConfig{ID: "loop", Transport: "sse", Endpoint: "http://127.0.0.1:1/sse"}
	hc := s.guardedHTTPClient(nil)
	if _, err := hc.Get(s.Endpoint); !errors.Is(err, egress.ErrBlockedAddr) {
		t.Fatalf("err = %v, want ErrBlockedAddr", err)
	}
}
