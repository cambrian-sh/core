package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0140 D4: per-call credential selection. The most important property in
// this file: a call made FOR principal A never carries principal B's token,
// on any transport, however the calls interleave.

type fakeUserCreds struct {
	mu        sync.Mutex
	tokens    map[string]string // server/principal → token
	reconnect map[string]bool
	// short: the principal's account lacks scopes the server declares.
	short map[string]bool
	calls int
}

func newFakeUserCreds() *fakeUserCreds {
	return &fakeUserCreds{tokens: map[string]string{}, reconnect: map[string]bool{}, short: map[string]bool{}}
}

func (f *fakeUserCreds) set(server, principal, tok string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens[server+"/"+principal] = tok
}

func (f *fakeUserCreds) Credential(_ context.Context, req domain.CredentialRequest) (domain.Credential, error) {
	server, principal := req.ServerID, req.PrincipalID
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.reconnect[server+"/"+principal] {
		return domain.Credential{}, domain.ErrCredentialReconnectNeeded
	}
	if f.short[server+"/"+principal] {
		return domain.Credential{}, fmt.Errorf("%w: missing Mail.Send", domain.ErrCredentialInsufficientScope)
	}
	tok, ok := f.tokens[server+"/"+principal]
	if !ok {
		return domain.Credential{}, domain.ErrCredentialNotConnected
	}
	return domain.Credential{Token: tok, Expiry: time.Now().Add(time.Hour)}, nil
}

func (f *fakeUserCreds) ConnectedPrincipals(server string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k := range f.tokens {
		if s, p, _ := strings.Cut(k, "/"); s == server {
			out = append(out, p)
		}
	}
	return out
}

// whoamiServer is a Streamable-HTTP MCP server whose one tool echoes the
// Authorization header of the request that carried the call. It also records
// every (session id, Authorization) pair it sees, for the no-mixing check.
type whoamiServer struct {
	url  string
	mu   sync.Mutex
	seen map[string]map[string]bool // Mcp-Session-Id → set of Authorization values
	hdrs []string
}

func newWhoamiServer(t *testing.T, header string) *whoamiServer {
	t.Helper()
	w := &whoamiServer{seen: map[string]map[string]bool{}}
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "whoami", Version: "1.0.0"}, nil)
	mcpsdk.AddTool(server, &mcpsdk.Tool{Name: "whoami", Description: "echo the credential"},
		func(_ context.Context, req *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, map[string]string, error) {
			return nil, map[string]string{"auth": req.Extra.Header.Get(header)}, nil
		})
	h := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil)
	ts := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		a := r.Header.Get(header)
		w.mu.Lock()
		w.hdrs = append(w.hdrs, a)
		if sid := r.Header.Get("Mcp-Session-Id"); sid != "" {
			if w.seen[sid] == nil {
				w.seen[sid] = map[string]bool{}
			}
			w.seen[sid][a] = true
		}
		w.mu.Unlock()
		h.ServeHTTP(rw, r)
	}))
	t.Cleanup(ts.Close)
	w.url = ts.URL
	return w
}

func (w *whoamiServer) sessionsMixed() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var bad []string
	for sid, auths := range w.seen {
		if len(auths) > 1 {
			bad = append(bad, sid)
		}
	}
	return bad
}

func callAs(ctx context.Context, h *Handler, principal, tool string) (string, error) {
	if principal != "" {
		ctx = domain.WithRunPrincipal(ctx, principal)
	}
	out, err := h.Execute(ctx, domain.ToolCall{ToolName: tool, ArgsJSON: []byte(`{}`)})
	return string(out), err
}

func perUserHTTP(id, url, inject, header string) ServerConfig {
	return ServerConfig{ID: id, Transport: "http", Endpoint: url, AllowPrivateNetwork: true,
		DefaultPolicy:   &ToolPolicy{Effects: []string{"read"}},
		CredentialScope: ScopePerUser, UserInject: inject, UserInjectName: header}
}

// THE test: interleaved concurrent calls for A and B each see only their own
// token, sessions never carry two principals' tokens, and the shared token is
// never used for a per_user server.
func TestPerUser_HTTP_ACallNeverUsesAnotherPrincipalsToken(t *testing.T) {
	srv := newWhoamiServer(t, "Authorization")
	creds := newFakeUserCreds()
	creds.set("mail", "alice", "tok-alice-1111")
	creds.set("mail", "bob", "tok-bob-2222")

	conn := NewConnector()
	defer conn.Close()
	conn.SetUserCredentials(creds, UserPoolLimits{Max: 8})
	cfg := perUserHTTP("mail", srv.url, "bearer", "")
	cfg.AuthType, cfg.AuthTokenEnv = "bearer", "SHARED_TOKEN_ENV_UNUSED"
	conn.rememberConfig(cfg)
	h := &Handler{Sessions: conn}

	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 20; i++ {
		for _, p := range []string{"alice", "bob"} {
			wg.Add(1)
			go func(p string) {
				defer wg.Done()
				out, err := callAs(context.Background(), h, p, "mcp:mail/whoami")
				if err != nil {
					errs <- err
					return
				}
				want := "Bearer tok-" + p
				if !strings.Contains(out, want) {
					errs <- errors.New(p + " got " + out)
				}
				other := map[string]string{"alice": "tok-bob", "bob": "tok-alice"}[p]
				if strings.Contains(out, other) {
					errs <- errors.New(p + " CROSSED to " + out)
				}
			}(p)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if bad := srv.sessionsMixed(); len(bad) > 0 {
		t.Fatalf("sessions carried more than one principal's token: %v", bad)
	}
	if got := conn.UserInstances(); len(got) != 2 {
		t.Fatalf("want one session per principal, got %v", got)
	}
}

func TestPerUser_HTTP_CustomHeaderInjection(t *testing.T) {
	srv := newWhoamiServer(t, "X-Api-Key")
	creds := newFakeUserCreds()
	creds.set("crm", "alice", "key-alice-9999")
	conn := NewConnector()
	defer conn.Close()
	conn.SetUserCredentials(creds, UserPoolLimits{})
	conn.rememberConfig(perUserHTTP("crm", srv.url, "header", "X-Api-Key"))
	out, err := callAs(context.Background(), &Handler{Sessions: conn}, "alice", "mcp:crm/whoami")
	if err != nil || !strings.Contains(out, `"key-alice-9999"`) {
		t.Fatalf("header injection: %q %v", out, err)
	}
}

// No credential ⇒ a clean, actionable error, and no fallback to anyone.
func TestPerUser_MissingCredentialIsACleanErrorWithNoFallback(t *testing.T) {
	srv := newWhoamiServer(t, "Authorization")
	creds := newFakeUserCreds()
	creds.set("mail", "alice", "tok-alice-1111")
	conn := NewConnector()
	defer conn.Close()
	conn.SetUserCredentials(creds, UserPoolLimits{})
	conn.rememberConfig(perUserHTTP("mail", srv.url, "bearer", ""))
	h := &Handler{Sessions: conn}

	_, err := callAs(context.Background(), h, "carol", "mcp:mail/whoami")
	var nc *NotConnectedError
	if !errors.As(err, &nc) || !strings.Contains(err.Error(), "Connect my accounts") {
		t.Fatalf("want an actionable not-connected error, got %v", err)
	}
	if strings.Contains(err.Error(), "alice") || strings.Contains(err.Error(), "tok-") {
		t.Fatalf("error leaks another principal or a token: %v", err)
	}
	// No principal on the call at all: refused, never served.
	if _, err := callAs(context.Background(), h, "", "mcp:mail/whoami"); !errors.As(err, &nc) || !nc.NoPerson {
		t.Fatalf("a call for no person must be refused: %v", err)
	}
	// Reconnect needed: a distinct message, still no fallback.
	creds.mu.Lock()
	creds.reconnect["mail/alice"] = true
	creds.mu.Unlock()
	if _, err := callAs(context.Background(), h, "alice", "mcp:mail/whoami"); !errors.As(err, &nc) || !nc.Reconnect {
		t.Fatalf("want reconnect-needed, got %v", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	for _, a := range srv.hdrs {
		if a != "" && a != "Bearer tok-alice-1111" {
			t.Fatalf("server saw an unexpected credential %q", a)
		}
	}
}

// either: the principal's own credential first; the shared session only when
// the policy allows it, and never for a dead grant.
func TestPerUser_EitherFallbackPolicy(t *testing.T) {
	shared := newWhoamiServer(t, "Authorization")
	creds := newFakeUserCreds()
	creds.set("gh", "alice", "tok-alice-1111")
	t.Setenv("GH_SHARED_TEST_TOKEN", "tok-shared-0000")

	run := func(fallback bool) (*Connector, *Handler) {
		conn := NewConnector()
		conn.SetUserCredentials(creds, UserPoolLimits{})
		cfg := ServerConfig{ID: "gh", Transport: "http", Endpoint: shared.url, AllowPrivateNetwork: true,
			AuthType: "bearer", AuthTokenEnv: "GH_SHARED_TEST_TOKEN",
			DefaultPolicy:   &ToolPolicy{Effects: []string{"read"}},
			CredentialScope: ScopeEither, UserInject: "bearer", SharedFallback: fallback}
		conn.ConnectAll(context.Background(), []ServerConfig{cfg}) // the shared session
		return conn, &Handler{Sessions: conn}
	}

	conn, h := run(true)
	if out, err := callAs(context.Background(), h, "alice", "mcp:gh/whoami"); err != nil || !strings.Contains(out, "tok-alice") {
		t.Fatalf("either: alice must use her own token: %q %v", out, err)
	}
	if out, err := callAs(context.Background(), h, "bob", "mcp:gh/whoami"); err != nil || !strings.Contains(out, "tok-shared") {
		t.Fatalf("either+fallback: bob falls back to shared: %q %v", out, err)
	}
	creds.mu.Lock()
	creds.reconnect["gh/alice"] = true
	creds.mu.Unlock()
	if _, err := callAs(context.Background(), h, "alice", "mcp:gh/whoami"); err == nil {
		t.Fatal("a dead grant must not silently fall back to the shared credential")
	}
	creds.mu.Lock()
	creds.reconnect["gh/alice"] = false
	creds.mu.Unlock()
	// An account short of the server's scopes: "needs more permission", and
	// never the shared credential (ADR-0141 grant scope enforcement).
	creds.mu.Lock()
	creds.short["gh/alice"] = true
	creds.mu.Unlock()
	_, err := callAs(context.Background(), h, "alice", "mcp:gh/whoami")
	var nc *NotConnectedError
	if !errors.As(err, &nc) || !nc.NeedsConsent || !strings.Contains(err.Error(), "Mail.Send") || strings.Contains(err.Error(), "tok-") {
		t.Fatalf("want a needs-consent refusal naming the scope, got %v", err)
	}
	creds.mu.Lock()
	creds.short["gh/alice"] = false
	creds.mu.Unlock()
	conn.Close()

	conn2, h2 := run(false)
	defer conn2.Close()
	if _, err := callAs(context.Background(), h2, "bob", "mcp:gh/whoami"); err == nil {
		t.Fatal("either WITHOUT shared_fallback must not use the shared credential")
	}
}

// stdio: one process per principal with exactly that principal's token (and
// not the server's shared secret_env); lazy start, idle kill, cap, and a
// relaunch when the token was refreshed.
func TestPerUser_Stdio_InstanceLifecycle(t *testing.T) {
	SetSecretResolver(fakeSecrets{"mcp:mail:env:SHARED_SECRET": "shared-secret-value"})
	defer SetSecretResolver(nil)
	creds := newFakeUserCreds()
	creds.set("mail", "alice", "tok-alice-1111")
	creds.set("mail", "bob", "tok-bob-2222")
	creds.set("mail", "carol", "tok-carol-3333")

	conn := NewConnector()
	defer conn.Close()
	now := time.Now()
	conn.users.now = func() time.Time { return now }
	conn.SetUserCredentials(creds, UserPoolLimits{Max: 2, Idle: time.Minute})
	cfg := envServer(t, "mail")
	cfg.SecretEnv = []string{"SHARED_SECRET"}
	cfg.CredentialScope, cfg.UserInject, cfg.UserInjectName = ScopePerUser, "env", "MAIL_TOKEN"
	conn.rememberConfig(cfg)
	h := &Handler{Sessions: conn}

	if got := conn.UserInstances(); len(got) != 0 {
		t.Fatalf("lazy: nothing may start before a call, got %v", got)
	}
	out, err := callAs(context.Background(), h, "alice", "mcp:mail/env")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "MAIL_TOKEN=tok-alice-1111") {
		t.Fatalf("alice's process lacks her token: %s", out)
	}
	if strings.Contains(out, "tok-bob") || strings.Contains(out, "shared-secret-value") {
		t.Fatalf("alice's process carries another credential: %s", out)
	}
	out, _ = callAs(context.Background(), h, "bob", "mcp:mail/env")
	if !strings.Contains(out, "MAIL_TOKEN=tok-bob-2222") || strings.Contains(out, "tok-alice") {
		t.Fatalf("bob's process: %s", out)
	}
	if got := conn.UserInstances(); len(got) != 2 {
		t.Fatalf("want 2 instances, got %v", got)
	}

	// Cap 2: carol evicts the least recently used idle one (alice).
	now = now.Add(time.Second)
	_, _ = callAs(context.Background(), h, "bob", "mcp:mail/env") // bob is now most recent
	now = now.Add(time.Second)
	if out, err := callAs(context.Background(), h, "carol", "mcp:mail/env"); err != nil || !strings.Contains(out, "tok-carol") {
		t.Fatalf("carol: %v", err)
	}
	if got := strings.Join(conn.UserInstances(), ","); got != "mail/bob,mail/carol" {
		t.Fatalf("cap/LRU: got %s", got)
	}

	// A refreshed token relaunches that principal's process with the new one.
	creds.set("mail", "bob", "tok-bob-NEW-4444")
	out, _ = callAs(context.Background(), h, "bob", "mcp:mail/env")
	if !strings.Contains(out, "MAIL_TOKEN=tok-bob-NEW-4444") {
		t.Fatalf("refreshed token not injected: %s", out)
	}

	// Idle kill.
	now = now.Add(2 * time.Minute)
	if n := conn.ReapIdleUsers(); n != 2 {
		t.Fatalf("idle reap closed %d, want 2", n)
	}
	if got := conn.UserInstances(); len(got) != 0 {
		t.Fatalf("after reap: %v", got)
	}
	// Disconnect retires a principal's instance.
	_, _ = callAs(context.Background(), h, "alice", "mcp:mail/env")
	conn.UserDisconnected("mail", "alice")
	if got := conn.UserInstances(); len(got) != 0 {
		t.Fatalf("after disconnect: %v", got)
	}
}

// A per_user server has no shared session: its tools are discovered through
// the first connected principal and published; health reads awaiting_user
// until then.
func TestPerUser_DiscoveryWaitsForTheFirstAccount(t *testing.T) {
	srv := newWhoamiServer(t, "Authorization")
	creds := newFakeUserCreds()
	conn := NewConnector()
	defer conn.Close()
	conn.SetUserCredentials(creds, UserPoolLimits{})
	sink := newRecSink()
	cfg := perUserHTTP("mail", srv.url, "bearer", "")
	if tools := conn.ConnectAll(context.Background(), []ServerConfig{cfg}); len(tools) != 0 {
		t.Fatalf("a per_user server must not connect a shared session at boot: %d tools", len(tools))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn.AddServer(ctx, cfg, sink, time.Hour)
	waitFor(t, "awaiting_user", func() bool { h, _ := conn.Health("mail"); return h.State == HealthAwaitingUser })

	creds.set("mail", "alice", "tok-alice-1111")
	conn.UserConnected("mail", "alice")
	waitFor(t, "tools published", func() bool { return sink.count("mail") == 1 })
	waitFor(t, "ready", func() bool { h, _ := conn.Health("mail"); return h.State == HealthReady })
	conn.RemoveServer(context.Background(), "mail", sink)
	if sink.count("mail") != 0 {
		t.Fatal("removing the server must withdraw the discovered tools")
	}
}
