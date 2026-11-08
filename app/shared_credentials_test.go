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
func (f *fakeOrgAccounts) ConnectedPrincipals(string) []string { return nil }
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
func (f *fakeOrgAccounts) ImportOrgCredential(ctx context.Context, r domain.CredentialRequest, v, _ string) (bool, error) {
	f.mu.Lock()
	_, bound := f.slots[f.key(r)]
	f.mu.Unlock()
	if bound {
		return false, nil
	}
	return true, f.SetOrgCredential(ctx, r, v, "")
}
func (f *fakeOrgAccounts) OrgCredentialStatus(r domain.CredentialRequest) (bool, string, time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.slots[f.key(r)]
	return ok, "", time.Unix(1700000000, 0)
}

// ADR-0141 phase 2 + 5 through the composition: with an org-account provider
// installed, the kernel's legacy writers land in the account, the connector
// reads the account (never the consumer-named store entry), the provider is
// told which legacy slots exist, and a clear also removes the legacy copy so
// the next boot's migration cannot resurrect it. With none installed (OSS)
// nothing changes.
func TestSharedCredentials_RoutedThroughTheOrgAccountProvider(t *testing.T) {
	store, err := storage.OpenConfigStore(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	mcp.SetSecretResolver(store)
	t.Cleanup(func() { mcp.SetSecretResolver(nil); mcp.SetOrgCredentials(nil) })

	// OSS first: the store, as before.
	if err := writeSharedMCPCredential(store, "gh", mcp.TokenSecretName("gh"), "store-token"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := sharedMCPCredentialStatus(store, "gh", mcp.TokenSecretName("gh")); !ok {
		t.Fatal("OSS: the store holds it")
	}

	rt := &mcpRuntime{}
	rt.upsertConfig(config.MCPServerConfig{ID: "gh", Transport: "http", Endpoint: "http://127.0.0.1:1",
		SecretEnv: []string{"GH_PAT"}}, mcpSourceStore)
	conn := mcp.NewConnector()
	defer conn.Close()
	fake := &fakeOrgAccounts{slots: map[string]string{}}
	var legacy []LegacySharedCredential
	factory := func(svc CredentialServices) (domain.CredentialSource, error) {
		legacy = svc.LegacySharedCredentials()
		// What the premium provider does at build: migrate.
		for _, l := range legacy {
			if v, ok := svc.ResolveNamedSecret(l.SecretName); ok {
				_, _ = fake.ImportOrgCredential(context.Background(),
					domain.CredentialRequest{ServerID: l.ServerID, Purpose: domain.CredentialPurposeMCP, Slot: l.Slot}, v, l.SecretName)
			}
		}
		return fake, nil
	}
	attachNamedSecretStoreForTest(t, store)
	if _, err := buildCredentialSource(factory, &config.Config{}, store, rt, conn, &credentialSeams{}); err != nil {
		t.Fatal(err)
	}
	if len(legacy) != 2 || legacy[0].SecretName != "mcp:gh:token" || legacy[1].SecretName != "mcp:gh:env:GH_PAT" {
		t.Fatalf("legacy slots: %+v", legacy)
	}
	if mcp.OrgCredentials() == nil {
		t.Fatal("an org-owning provider must be installed for shared servers")
	}
	if fake.slots["gh/token"] != "store-token" {
		t.Fatalf("migration: %v", fake.slots)
	}

	// A legacy write now rotates the account; the store entry is not touched.
	if err := writeSharedMCPCredential(store, "gh", mcp.TokenSecretName("gh"), "account-token"); err != nil {
		t.Fatal(err)
	}
	if fake.slots["gh/token"] != "account-token" {
		t.Fatal("the write must land in the org account")
	}
	if v := store.Resolve(mcp.TokenSecretName("gh"), ""); v != "store-token" {
		t.Fatalf("the legacy copy is left for rollback, got %q", v)
	}
	// The env slot writer.
	if err := writeSharedMCPCredential(store, "gh", mcp.EnvSecretName("gh", "GH_PAT"), "pat"); err != nil {
		t.Fatal(err)
	}
	if fake.slots["gh/env:GH_PAT"] != "pat" {
		t.Fatalf("env slot: %v", fake.slots)
	}
	// Clear unbinds AND drops the legacy copy.
	if err := clearSharedMCPCredential(store, "gh", mcp.TokenSecretName("gh")); err != nil {
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
