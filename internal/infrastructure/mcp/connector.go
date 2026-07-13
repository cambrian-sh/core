package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cambrian-sh/core/domain"
)

// ServerConfig is one MCP server's connection info, mapped from operator config
// at the composition root (ADR-0043 D2/D9). Auth secrets are read from the named
// environment variable, never inlined.
type ServerConfig struct {
	ID           string
	Transport    string   // "stdio" | "http"
	Endpoint     string   // URL (http) or command (stdio)
	Args         []string // stdio command args
	AuthType     string   // "none" | "bearer" | "header"
	AuthHeader   string // header name when AuthType == "header"
	AuthTokenEnv string // env var holding the credential
	// Tools is operator policy keyed by the server's advertised (unqualified)
	// tool name. Applied to the discovered SystemTool — the server's own metadata
	// is descriptive only (A1.5).
	Tools map[string]ToolPolicy
}

// ToolPolicy is the operator-set policy for one discovered tool (ADR-0043 D4/D9).
type ToolPolicy struct {
	Dangerous      bool
	DataWriteKinds []string
}

// Connector dials configured MCP servers, discovers their tools, and serves the
// live client sessions to the Handler (it implements SessionSource). It is the
// only place outside this package's Handler that touches the SDK. One warm
// session per server; an unreachable server is skipped (graceful degradation).
type Connector struct {
	client   *mcpsdk.Client
	mu       sync.RWMutex
	sessions map[string]*mcpsdk.ClientSession
}

// NewConnector constructs an empty connector with a single shared MCP client.
func NewConnector() *Connector {
	return &Connector{
		client:   mcpsdk.NewClient(&mcpsdk.Implementation{Name: "cambrian", Version: "1.0.0"}, nil),
		sessions: map[string]*mcpsdk.ClientSession{},
	}
}

// Session implements SessionSource.
func (c *Connector) Session(serverID string) (*mcpsdk.ClientSession, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s, ok := c.sessions[serverID]
	return s, ok
}

// ConnectAll dials every configured server, discovers its tools, and returns them
// as namespaced SystemTools (mcp:<id>/<tool>) for registration. A server that
// fails to connect or discover is logged and skipped — it never fails the others
// or the kernel (ADR-0043 D8).
func (c *Connector) ConnectAll(ctx context.Context, servers []ServerConfig) []domain.SystemTool {
	var tools []domain.SystemTool
	for _, s := range servers {
		sess, err := c.connect(ctx, s)
		if err != nil {
			slog.Warn("ADR-0043: MCP server connect failed (skipping)", "server", s.ID, "err", err)
			continue
		}
		c.mu.Lock()
		c.sessions[s.ID] = sess
		c.mu.Unlock()

		discovered, derr := discover(ctx, s.ID, sess, s.Tools)
		if derr != nil {
			slog.Warn("ADR-0043: MCP tool discovery failed", "server", s.ID, "err", derr)
			continue
		}
		slog.Info("ADR-0043: MCP server connected", "server", s.ID, "tools", len(discovered))
		tools = append(tools, discovered...)
	}
	return tools
}

func (c *Connector) connect(ctx context.Context, s ServerConfig) (*mcpsdk.ClientSession, error) {
	var transport mcpsdk.Transport
	switch s.Transport {
	case "http": // Streamable HTTP (2025-03-26 spec) — single endpoint, POST + optional SSE
		transport = &mcpsdk.StreamableClientTransport{
			Endpoint:   s.Endpoint,
			HTTPClient: s.httpClient(),
			// Stateless Streamable-HTTP servers (e.g. FastMCP httpStream stateless:true,
			// which self-hosted Firecrawl uses) don't serve the optional standalone
			// server→client SSE GET stream. Disable it so init doesn't fail on the
			// follow-up GET; request/response still works (responses ride the POST).
			DisableStandaloneSSE: true,
		}
	case "sse": // legacy HTTP+SSE (2024-11-05 spec) — e.g. self-hosted Firecrawl's /sse
		transport = &mcpsdk.SSEClientTransport{Endpoint: s.Endpoint, HTTPClient: s.httpClient()}
	case "stdio":
		transport = &mcpsdk.CommandTransport{Command: exec.Command(s.Endpoint, s.Args...)} //nolint:gosec // operator-configured command
	default:
		return nil, fmt.Errorf("unknown transport %q (want stdio|http|sse)", s.Transport)
	}
	return c.client.Connect(ctx, transport, nil)
}

// httpClient builds the HTTP client for an http/sse transport, injecting the
// static credential when configured (ADR-0043 D9).
func (s ServerConfig) httpClient() *http.Client {
	c := &http.Client{}
	if s.AuthType != "" && s.AuthType != "none" {
		c.Transport = &authRoundTripper{
			base:     http.DefaultTransport,
			authType: s.AuthType,
			header:   s.AuthHeader,
			token:    os.Getenv(s.AuthTokenEnv),
		}
	}
	return c
}

// ToolSink receives a server's current tool set on each (re)discovery, and a
// removal when a server drops (ADR-0043 D8 health-gating / ADR-0044 re-sync). The
// kernel implements it to keep the tool registry AND the semantic-retrieval index
// in sync — registering/de-registering tools and adding/removing their vectors.
type ToolSink interface {
	SetServerTools(ctx context.Context, serverID string, tools []domain.SystemTool)
	RemoveServerTools(ctx context.Context, serverID string)
}

// Watch runs a background health + reconnect loop per server (ADR-0043 D8),
// blocking until ctx is done. For a connected server it pings every interval; on
// failure it drops the server's tools (sink.RemoveServerTools → they leave the
// menu) and reconnects with exponential backoff, re-discovering and re-publishing
// on success (sink.SetServerTools → re-registered + re-indexed, ADR-0044). A
// server unreachable at boot is retried here until it comes up.
func (c *Connector) Watch(ctx context.Context, servers []ServerConfig, sink ToolSink, interval time.Duration) {
	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Add(1)
		go func(s ServerConfig) {
			defer wg.Done()
			c.watchServer(ctx, s, sink, interval)
		}(s)
	}
	wg.Wait()
}

func (c *Connector) watchServer(ctx context.Context, s ServerConfig, sink ToolSink, interval time.Duration) {
	const maxBackoff = 30 * time.Second
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		if sess, ok := c.Session(s.ID); ok {
			// Healthy path: wait, then ping.
			if !sleepCtx(ctx, interval) {
				return
			}
			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := sess.Ping(pctx, nil)
			cancel()
			if err == nil {
				continue // still healthy
			}
			slog.Warn("ADR-0043: MCP server unhealthy — dropping tools, will reconnect", "server", s.ID, "err", err)
			sink.RemoveServerTools(ctx, s.ID)
			c.mu.Lock()
			delete(c.sessions, s.ID)
			c.mu.Unlock()
			_ = sess.Close()
			backoff = time.Second
			continue
		}
		// Reconnect path.
		newSess, err := c.connect(ctx, s)
		if err != nil {
			if !sleepCtx(ctx, backoff) {
				return
			}
			if backoff < maxBackoff {
				backoff *= 2
			}
			continue
		}
		c.mu.Lock()
		c.sessions[s.ID] = newSess
		c.mu.Unlock()
		tools, derr := discover(ctx, s.ID, newSess, s.Tools)
		if derr != nil {
			slog.Warn("ADR-0043: MCP re-discovery failed", "server", s.ID, "err", derr)
			continue
		}
		sink.SetServerTools(ctx, s.ID, tools)
		slog.Info("ADR-0043: MCP server (re)synced", "server", s.ID, "tools", len(tools))
		backoff = time.Second
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Close closes all live sessions.
func (c *Connector) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, s := range c.sessions {
		_ = s.Close()
		delete(c.sessions, id)
	}
}

// discover lists a server's tools and maps them to namespaced SystemTools. The
// advertised metadata is descriptive only — policy is bound by operator config
// keyed by the identity (A1.5 / ADR-0043 D3).
func discover(ctx context.Context, serverID string, sess *mcpsdk.ClientSession, policy map[string]ToolPolicy) ([]domain.SystemTool, error) {
	res, err := sess.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	out := make([]domain.SystemTool, 0, len(res.Tools))
	for _, t := range res.Tools {
		var schema json.RawMessage
		if t.InputSchema != nil {
			if b, merr := json.Marshal(t.InputSchema); merr == nil {
				schema = b
			}
		}
		st := domain.SystemTool{
			Name:        ToolNamePrefix + serverID + "/" + t.Name,
			Description: t.Description,
			Schema:      schema,
		}
		// Apply operator policy (dangerous-flag + data-egress kinds) — never the
		// server's own advertised metadata (A1.5 / ADR-0043 D4/D9).
		if p, ok := policy[t.Name]; ok {
			st.Dangerous = p.Dangerous
			st.DataWriteKinds = p.DataWriteKinds
		}
		out = append(out, st)
	}
	return out, nil
}

// SlogEgressAuditor is the default EgressAuditor — it logs each remote-tool data
// egress (ADR-0043 D4). Operators can replace it with a structured sink.
type SlogEgressAuditor struct{}

// RecordEgress implements domain.EgressAuditor.
func (SlogEgressAuditor) RecordEgress(agentID, toolName string, dataClasses []string) {
	slog.Info("ADR-0043: MCP data egress",
		"agent_id", agentID, "tool", toolName, "data_classes", dataClasses)
}

// authRoundTripper injects a static credential on every request to a server
// (ADR-0043 D9 static auth). The credential is bound to this server's transport
// and never reaches another.
type authRoundTripper struct {
	base     http.RoundTripper
	authType string
	header   string
	token    string
}

func (a *authRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	switch a.authType {
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+a.token)
	case "header":
		if a.header != "" {
			req.Header.Set(a.header, a.token)
		}
	}
	return a.base.RoundTrip(req)
}
