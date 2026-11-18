package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/mcp"
	"github.com/cambrian-sh/core/internal/storage"
)

// fakeAccounts is a provider built through the ADR-0141 seam: it records what
// the kernel handed it and serves one token per (server, principal).
type fakeAccounts struct {
	mu     sync.Mutex
	tokens map[string]string
	svc    CredentialServices
}

func (f *fakeAccounts) Credential(_ context.Context, req domain.CredentialRequest) (domain.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.Purpose != domain.CredentialPurposeMCP {
		return domain.Credential{}, errors.New("unexpected purpose " + req.Purpose)
	}
	tok, ok := f.tokens[req.ServerID+"/"+req.PrincipalID]
	if !ok {
		return domain.Credential{}, domain.ErrCredentialNotConnected
	}
	return domain.Credential{Token: tok, Expiry: time.Now().Add(time.Hour)}, nil
}

func (f *fakeAccounts) ConnectedPrincipals(server string) []string {
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

// ADR-0141 end to end through the composition: a provider registered through
// the seam is built with the kernel's services and installed on the live
// connector; alice's call to a per_user Streamable-HTTP server carries HER
// token, bob's is refused as not connected and the server never sees a
// credential for him; removing alice's account (CredentialChanged) retires
// her session, so her next call is refused too.
func TestCredentialSource_EndToEndThroughTheSeam(t *testing.T) {
	var smu sync.Mutex
	var seen []string
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "whoami", Version: "1"}, nil)
	mcpsdk.AddTool(server, &mcpsdk.Tool{Name: "whoami"},
		func(_ context.Context, req *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, map[string]string, error) {
			return nil, map[string]string{"auth": req.Extra.Header.Get("Authorization")}, nil
		})
	sh := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, nil)
	mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		smu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		smu.Unlock()
		sh.ServeHTTP(w, r)
	}))
	defer mcpSrv.Close()

	store, err := storage.OpenConfigStore(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	entry := config.MCPServerConfig{ID: "gh", Transport: "http", Endpoint: mcpSrv.URL, AllowPrivateNetwork: true, DisplayName: "GitHub",
		DefaultToolPolicy: &config.MCPToolConfig{Effects: []string{"read"}},
		UserCredential: &config.MCPUserCredentialConfig{Scope: "per_user", Provider: "github",
			Scopes: []string{"repo"}, Inject: "bearer"}}
	rt := &mcpRuntime{}
	rt.upsertConfig(entry, mcpSourceStore)
	fake := &fakeAccounts{tokens: map[string]string{"gh/alice": "token-of-alice-777"}}
	factory := func(svc CredentialServices) (domain.CredentialSource, error) {
		fake.svc = svc
		return fake, nil
	}
	// As bootstrapKernel does: the source is built first and the connector is
	// constructed with it (the provider reaches the connector lazily).
	var conn *mcp.Connector
	src, err := buildCredentialSource(factory, &config.Config{}, store,
		func() *mcpRuntime { return rt }, func() *mcp.Connector { return conn }, &credentialSeams{})
	if err != nil || src == nil {
		t.Fatalf("build: %v %v", src, err)
	}
	conn = mcp.NewConnector(mcpCredentialSources(&config.Config{}, store, src))
	defer conn.Close()
	// The services the provider gets.
	m, ok := fake.svc.ServerCredentialModel("gh")
	if !ok || m.Scope != domain.CredentialScopePerUser || m.Provider != "github" || m.DisplayName != "GitHub" {
		t.Fatalf("model: %+v %v", m, ok)
	}
	if ps := fake.svc.PersonalServers(); len(ps) != 1 || ps[0].ServerID != "gh" {
		t.Fatalf("personal servers: %+v", ps)
	}
	if err := fake.svc.Records.PutSealed(context.Background(), "k", []byte("v")); err != nil {
		t.Fatalf("records: %v", err)
	}
	if _, err := fake.svc.RunMutation(context.Background(), "c", "r", "a", "t", "id", func() error { return nil }); err == nil {
		t.Fatal("a plugin mutation before the operator plane exists must be refused")
	}

	conn.ConnectAll(context.Background(), []mcp.ServerConfig{mcpServerFromConfig(entry, rt.sandbox)})
	h := &mcp.Handler{Sessions: conn}
	call := func(principal string) (string, error) {
		ctx := domain.WithRunPrincipal(context.Background(), principal)
		out, err := h.Execute(ctx, domain.ToolCall{ToolName: "mcp:gh/whoami", ArgsJSON: []byte(`{}`)})
		return string(out), err
	}
	if out, err := call("alice"); err != nil || !strings.Contains(out, "Bearer token-of-alice-777") {
		t.Fatalf("alice's call: %q %v", out, err)
	}
	var nc *mcp.NotConnectedError
	if _, err := call("bob"); !errors.As(err, &nc) {
		t.Fatalf("bob's call must be refused as not connected: %v", err)
	}
	smu.Lock()
	for _, a := range seen {
		if a != "Bearer token-of-alice-777" {
			t.Fatalf("the MCP server saw an unexpected credential %q", a)
		}
	}
	smu.Unlock()

	fake.mu.Lock()
	delete(fake.tokens, "gh/alice")
	fake.mu.Unlock()
	fake.svc.CredentialChanged("gh", "alice", false)
	if got := conn.UserInstances(); len(got) != 0 {
		t.Fatalf("a removed account's session must be retired: %v", got)
	}
	if _, err := call("alice"); !errors.As(err, &nc) {
		t.Fatalf("after removal: %v", err)
	}
}

// OSS default: no factory ⇒ nothing installed, and a factory without the
// config store is refused (per-user servers then fail closed).
func TestCredentialSource_OffAndMisconfigured(t *testing.T) {
	rt := func() *mcpRuntime { return &mcpRuntime{} }
	conn := func() *mcp.Connector { return nil }
	if src, err := buildCredentialSource(nil, &config.Config{}, nil, rt, conn, &credentialSeams{}); src != nil || err != nil {
		t.Fatalf("no factory must be (nil, nil): %v %v", src, err)
	}
	f := func(CredentialServices) (domain.CredentialSource, error) { return &fakeAccounts{}, nil }
	if _, err := buildCredentialSource(f, &config.Config{}, nil, rt, conn, &credentialSeams{}); err == nil {
		t.Fatal("a provider without the config store must fail")
	}
	r := &Registry{}
	if err := r.SetCredentialSource("a", f); err != nil {
		t.Fatal(err)
	}
	if err := r.SetCredentialSource("b", f); err == nil {
		t.Fatal("a second credential source must be refused")
	}
}

// Boot-time account audit entries (the provider's migration and legacy
// cleanup run before the operator plane is bound) are queued and written,
// in order, when it is — they used to be logged and dropped.
func TestCredentialSeams_AuditsBeforeBindAreFlushedOnBind(t *testing.T) {
	seams := &credentialSeams{}
	ctx, cancel := context.WithCancel(context.Background())
	_ = seams.recordAudit(ctx, domain.AuditEntry{ActionType: "migrate_org_account", TargetID: "a"})
	_ = seams.recordAudit(ctx, domain.AuditEntry{ActionType: "delete_legacy_credential", TargetID: "b"})
	cancel() // a cancelled boot context must not lose the queued entries

	var got []string
	sink := func(c context.Context, e domain.AuditEntry) error {
		if c.Err() != nil {
			t.Errorf("flushed with a cancelled context")
		}
		got = append(got, e.ActionType+":"+e.TargetID)
		return nil
	}
	seams.bindAudit(sink, nil)
	if strings.Join(got, ",") != "migrate_org_account:a,delete_legacy_credential:b" {
		t.Fatalf("flushed %v", got)
	}
	// After bind: written straight through, nothing queued.
	_ = seams.recordAudit(context.Background(), domain.AuditEntry{ActionType: "x", TargetID: "c"})
	if len(got) != 3 || len(seams.pending) != 0 {
		t.Fatalf("after bind: got %v, pending %d", got, len(seams.pending))
	}
}

func TestCredentialSeams_PreBindQueueIsBounded(t *testing.T) {
	seams := &credentialSeams{}
	for i := 0; i < maxPendingAudits+10; i++ {
		_ = seams.recordAudit(context.Background(), domain.AuditEntry{ActionType: "x"})
	}
	if len(seams.pending) != maxPendingAudits {
		t.Fatalf("pending %d, want the bound %d", len(seams.pending), maxPendingAudits)
	}
}
