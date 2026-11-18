package app

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/mcp"
	"github.com/cambrian-sh/core/internal/storage"
)

// fakeOrgAccounts is a provider that owns ORG credentials (ADR-0141 phase 2):
// it implements domain.OrgCredentialWriter, which is what makes the kernel
// route shared MCP credentials through it.
type fakeOrgAccounts struct {
	mu    sync.Mutex
	slots map[string]string // "<server>/<slot>" → value
}

func (f *fakeOrgAccounts) key(r domain.CredentialRequest) string {
	return r.ServerID + "/" + r.EffectiveSlot()
}

func (f *fakeOrgAccounts) Credential(_ context.Context, r domain.CredentialRequest) (domain.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.PrincipalID != "" {
		return domain.Credential{}, domain.ErrCredentialNotConnected
	}
	v, ok := f.slots[f.key(r)]
	if !ok {
		return domain.Credential{}, domain.ErrCredentialNotConnected
	}
	return domain.Credential{Token: v}, nil
}
func (f *fakeOrgAccounts) ConnectedPrincipals(context.Context, string) []string { return nil }
func (f *fakeOrgAccounts) SetOrgCredential(_ context.Context, r domain.CredentialRequest, v, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.slots[f.key(r)] = v
	return nil
}
func (f *fakeOrgAccounts) ClearOrgCredential(_ context.Context, r domain.CredentialRequest, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.slots[f.key(r)]
	delete(f.slots, f.key(r))
	return ok, nil
}
func (f *fakeOrgAccounts) OrgCredentialStatus(_ context.Context, r domain.CredentialRequest) (bool, string, time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.slots[f.key(r)]
	return ok, "", time.Unix(1700000000, 0)
}

// ADR-0141 phase 2 + 5 through the composition: with an org-account provider
// installed, the kernel's legacy writers land in the account, the connector
// reads the account (never the consumer-named store entry), and a clear also
// removes the stale consumer-named copy. With none installed (OSS) nothing
// changes. (The one-release boot migration was removed, ENG-661: the provider
// is no longer told which legacy slots exist.)
func TestSharedCredentials_RoutedThroughTheOrgAccountProvider(t *testing.T) {
	store, err := storage.OpenConfigStore(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	// OSS first (no provider ⇒ no org writer): the store, as before.
	if err := writeSharedMCPCredential(context.Background(), nil, store, "gh", mcp.TokenSecretName("gh"), "store-token"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := sharedMCPCredentialStatus(context.Background(), nil, store, "gh", mcp.TokenSecretName("gh")); !ok {
		t.Fatal("OSS: the store holds it")
	}

	rt := &mcpRuntime{}
	rt.upsertConfig(config.MCPServerConfig{ID: "gh", Transport: "http", Endpoint: "http://127.0.0.1:1",
		SecretEnv: []string{"GH_PAT"}}, mcpSourceStore)
	fake := &fakeOrgAccounts{slots: map[string]string{}}
	// An account already bound to the slot (what an operator binds today).
	_ = fake.SetOrgCredential(context.Background(),
		domain.CredentialRequest{ServerID: "gh", Purpose: domain.CredentialPurposeMCP, Slot: domain.SlotToken}, "store-token", "")
	factory := func(svc CredentialServices) (domain.CredentialSource, error) { return fake, nil }
	attachNamedSecretStoreForTest(t, store)
	seams := &credentialSeams{}
	src, err := buildCredentialSource(factory, &config.Config{}, store,
		func() *mcpRuntime { return rt }, func() *mcp.Connector { return nil }, seams)
	if err != nil {
		t.Fatal(err)
	}
	if cs := mcpCredentialSources(&config.Config{}, store, src); cs.Org == nil || cs.Secrets == nil {
		t.Fatalf("an org-owning provider must serve shared servers on the connector: %+v", cs)
	}
	org := seams.orgWriter()
	if org == nil {
		t.Fatal("the writers must reach the same provider's org accounts")
	}
	if fake.slots["gh/token"] != "store-token" {
		t.Fatalf("bound account: %v", fake.slots)
	}

	// A legacy write now rotates the account; the store entry is not touched.
	if err := writeSharedMCPCredential(context.Background(), org, store, "gh", mcp.TokenSecretName("gh"), "account-token"); err != nil {
		t.Fatal(err)
	}
	if fake.slots["gh/token"] != "account-token" {
		t.Fatal("the write must land in the org account")
	}
	if v := store.Resolve(mcp.TokenSecretName("gh"), ""); v != "store-token" {
		t.Fatalf("the legacy copy is left for rollback, got %q", v)
	}
	// The env slot writer.
	if err := writeSharedMCPCredential(context.Background(), org, store, "gh", mcp.EnvSecretName("gh", "GH_PAT"), "pat"); err != nil {
		t.Fatal(err)
	}
	if fake.slots["gh/env:GH_PAT"] != "pat" {
		t.Fatalf("env slot: %v", fake.slots)
	}
	// Clear unbinds AND drops the legacy copy.
	if err := clearSharedMCPCredential(context.Background(), org, store, "gh", mcp.TokenSecretName("gh")); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.slots["gh/token"]; ok {
		t.Fatal("clear must unbind")
	}
	if store.Configured(mcp.TokenSecretName("gh")) {
		t.Fatal("clear must remove the legacy copy, or the next boot's migration resurrects it")
	}
}

func attachNamedSecretStoreForTest(t *testing.T, store *storage.BoltConfigStore) {
	t.Helper()
	namedSecretMu.Lock()
	prevSrc, prevAdm := namedSecretSrc, namedSecretAdm
	namedSecretSrc, namedSecretAdm = store, store
	namedSecretMu.Unlock()
	t.Cleanup(func() {
		namedSecretMu.Lock()
		namedSecretSrc, namedSecretAdm = prevSrc, prevAdm
		namedSecretMu.Unlock()
	})
}

type sharedCredCtxKey struct{}

// ctxRecordingOrgAccounts records the sharedCredCtxKey value each org write sees.
type ctxRecordingOrgAccounts struct {
	fakeOrgAccounts
	seen []any
}

func (f *ctxRecordingOrgAccounts) SetOrgCredential(ctx context.Context, r domain.CredentialRequest, v, by string) error {
	f.seen = append(f.seen, ctx.Value(sharedCredCtxKey{}))
	return f.fakeOrgAccounts.SetOrgCredential(ctx, r, v, by)
}

func (f *ctxRecordingOrgAccounts) ClearOrgCredential(ctx context.Context, r domain.CredentialRequest, by string) (bool, error) {
	f.seen = append(f.seen, ctx.Value(sharedCredCtxKey{}))
	return f.fakeOrgAccounts.ClearOrgCredential(ctx, r, by)
}

// ENG-1175: the shared-credential writes run under the operator RPC's ctx, not
// context.Background(), so the provider sees its deadline and values.
func TestSharedCredentials_WritesCarryTheCallerContext(t *testing.T) {
	fake := &ctxRecordingOrgAccounts{fakeOrgAccounts: fakeOrgAccounts{slots: map[string]string{}}}
	ctx := context.WithValue(context.Background(), sharedCredCtxKey{}, "rpc")
	if err := writeSharedMCPCredential(ctx, fake, nil, "gh", mcp.TokenSecretName("gh"), "tok"); err != nil {
		t.Fatal(err)
	}
	if err := clearSharedMCPCredential(ctx, fake, nil, "gh", mcp.TokenSecretName("gh")); err != nil {
		t.Fatal(err)
	}
	if len(fake.seen) != 2 || fake.seen[0] != "rpc" || fake.seen[1] != "rpc" {
		t.Fatalf("provider saw ctx values %v, want the RPC's on both writes", fake.seen)
	}
}

// ctxProbeOrg is an org-credential writer that records whether
// OrgCredentialStatus received the caller's ctx (ENG-2044: the port is
// ctx-first, and the operator reads thread the RPC ctx down to it).
type ctxProbeOrg struct {
	mu      sync.Mutex
	calls   int
	dropped int
}

type orgProbeKey struct{}

func (p *ctxProbeOrg) SetOrgCredential(context.Context, domain.CredentialRequest, string, string) error {
	return nil
}
func (p *ctxProbeOrg) ClearOrgCredential(context.Context, domain.CredentialRequest, string) (bool, error) {
	return false, nil
}
func (p *ctxProbeOrg) OrgCredentialStatus(ctx context.Context, _ domain.CredentialRequest) (bool, string, time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if ctx.Value(orgProbeKey{}) != "rpc" {
		p.dropped++
	}
	return true, "0001", time.Unix(1700000000, 0)
}

// The operator plane's MCP-server read (ListMCPServers → mcpLister.MCPServers)
// reports a server's token through the org account provider with the request's
// ctx, not a fresh one.
func TestMCPServerRead_PassesTheRequestContextToTheOrgCredentialStatus(t *testing.T) {
	rt := &mcpRuntime{}
	rt.upsertConfig(config.MCPServerConfig{ID: "gh", Transport: "http", Endpoint: "http://127.0.0.1:1"}, mcpSourceStore)
	org := &ctxProbeOrg{}
	ctx := context.WithValue(context.Background(), orgProbeKey{}, "rpc")
	servers := mcpLister{runtime: rt, org: org}.MCPServers(ctx)
	if len(servers) != 1 || !servers[0].TokenConfigured || servers[0].TokenSource != "account" {
		t.Fatalf("servers = %+v, want gh with its token from the org account", servers)
	}
	if ok, _ := sharedMCPCredentialStatus(ctx, org, nil, "gh", mcp.TokenSecretName("gh")); !ok {
		t.Fatal("sharedMCPCredentialStatus: the bound account was not reported")
	}
	org.mu.Lock()
	defer org.mu.Unlock()
	if org.calls < 2 || org.dropped != 0 {
		t.Fatalf("OrgCredentialStatus calls = %d, %d without the request ctx", org.calls, org.dropped)
	}
}
