package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

type mapResolver map[string]string

func (m mapResolver) Resolve(name, _ string) string { return m[name] }

type orgOnly map[string]string

func (o orgOnly) Credential(_ context.Context, r domain.CredentialRequest) (domain.Credential, error) {
	if v, ok := o[r.ServerID+"/"+r.EffectiveSlot()]; ok && r.PrincipalID == "" {
		return domain.Credential{Token: v}, nil
	}
	return domain.Credential{}, domain.ErrCredentialNotConnected
}
func (orgOnly) ConnectedPrincipals(string) []string { return nil }

// ADR-0141 phase 5: once an org-account provider owns shared credentials,
// the consumer-named store entries are NOT read (one answer, not two); the
// deployment's environment still wins; without a provider, the store serves
// exactly as before (OSS).
func TestSharedCredential_OrgAccountReplacesTheStoreRead(t *testing.T) {
	oss := CredentialSources{Secrets: mapResolver{"mcp:gh:token": "store-tok", "mcp:pg:env:PGPASSWORD": "store-pw"}}
	if got := oss.tokenFor(context.Background(), "gh", ""); got != "store-tok" {
		t.Fatalf("OSS: %q", got)
	}
	org := oss
	org.Org = orgOnly{"gh/token": "account-tok"}
	if got := org.tokenFor(context.Background(), "gh", ""); got != "account-tok" {
		t.Fatalf("org account: %q", got)
	}
	if got := org.envSecretFor(context.Background(), "pg", "PGPASSWORD", ""); got != "" {
		t.Fatalf("an unbound slot must not fall back to the legacy store entry, got %q", got)
	}
	t.Setenv("GH_TOKEN_ENV", "env-tok")
	if got := org.tokenFor(context.Background(), "gh", "GH_TOKEN_ENV"); got != "env-tok" {
		t.Fatalf("the environment still wins: %q", got)
	}
}

// ctxProbe is an org-credential provider that records the context it was asked
// under and, when block is set, waits until that context ends.
type ctxProbe struct {
	block bool
	got   chan context.Context
}

type probeKey struct{}

func (p ctxProbe) Credential(ctx context.Context, _ domain.CredentialRequest) (domain.Credential, error) {
	p.got <- ctx
	if p.block {
		<-ctx.Done()
		return domain.Credential{}, ctx.Err()
	}
	return domain.Credential{Token: "account-tok"}, nil
}
func (ctxProbe) ConnectedPrincipals(string) []string { return nil }

// T-20261113-mcp-credential-source-and-secret-resolver-are: the provider is asked
// under the connect's own context (its values and cancellation), always with a
// deadline, so a hung provider can no longer stall a server connect forever.
func TestOrgCredential_AskedUnderTheCallersBoundedContext(t *testing.T) {
	p := ctxProbe{got: make(chan context.Context, 1)}
	ctx := context.WithValue(context.Background(), probeKey{}, "trace-1")
	if got := (CredentialSources{Org: p}).tokenFor(ctx, "gh", ""); got != "account-tok" {
		t.Fatalf("token = %q", got)
	}
	asked := <-p.got
	if asked.Value(probeKey{}) != "trace-1" {
		t.Error("the caller's context values did not reach the provider")
	}
	if _, ok := asked.Deadline(); !ok {
		t.Error("the provider was asked without a deadline")
	}

	// A hung provider returns "" as soon as the caller's context ends.
	hung := ctxProbe{block: true, got: make(chan context.Context, 1)}
	cctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan string, 1)
	go func() { done <- (CredentialSources{Org: hung}).envSecretFor(cctx, "pg", "PGPASSWORD", "") }()
	select {
	case got := <-done:
		if got != "" {
			t.Fatalf("a timed-out ask must yield no credential, got %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a hung provider stalled the credential lookup past the caller's deadline")
	}
}

// T-20261113-mcp-credential-source-and-secret-resolver-are: credential sources
// are constructor arguments, so the FIRST connect (the boot ConnectAll) already
// presents the stored token — when the store was a package global installed
// after bootstrap, the boot connect ran with none — and two connectors never
// share sources (the globals made a test's or a probe's store everyone's).
func TestConnector_CredentialsFromConstructionReachTheBootConnect(t *testing.T) {
	srv := newWhoamiServer(t, "Authorization")
	cfg := ServerConfig{ID: "gh", Transport: "http", Endpoint: srv.url, AllowPrivateNetwork: true,
		AuthType: "bearer", DefaultPolicy: &ToolPolicy{Effects: []string{"read"}}}

	withStore := NewConnector(CredentialSources{Secrets: mapResolver{TokenSecretName("gh"): "stored-tok-1"}})
	defer withStore.Close()
	bare := NewConnector(CredentialSources{})
	defer bare.Close()

	if tools := withStore.ConnectAll(context.Background(), []ServerConfig{cfg}); len(tools) == 0 {
		t.Fatal("boot connect failed")
	}
	out, err := callAs(context.Background(), &Handler{Sessions: withStore}, "", "mcp:gh/whoami")
	if err != nil || !strings.Contains(out, "stored-tok-1") {
		t.Fatalf("the boot connect did not present the constructed store's token: %q %v", out, err)
	}
	// The other connector, built without a store, must not see that token.
	if got := bare.creds.tokenFor(context.Background(), "gh", ""); got != "" {
		t.Fatalf("a connector built without a store resolved %q (shared state)", got)
	}
	// A probe through the bare connector (the console's "Test") presents no
	// token: it resolves from ITS connector's sources, not a process global.
	bare.Probe(context.Background(), cfg)
	srv.mu.Lock()
	defer srv.mu.Unlock()
	var withTok, without int
	for _, a := range srv.hdrs {
		switch a {
		case "Bearer stored-tok-1":
			withTok++
		case "", "Bearer": // an empty credential
			without++
		default:
			t.Fatalf("the server saw an unexpected credential %q", a)
		}
	}
	if withTok == 0 || without == 0 {
		t.Fatalf("want requests with the stored token (boot connect) and without (bare probe): %d/%d", withTok, without)
	}
}
