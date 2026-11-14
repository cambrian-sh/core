package mcp

import (
	"context"
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
	SetSecretResolver(mapResolver{"mcp:gh:token": "store-tok", "mcp:pg:env:PGPASSWORD": "store-pw"})
	t.Cleanup(func() { SetSecretResolver(nil); SetOrgCredentials(nil) })

	if got := tokenFor(context.Background(), "gh", ""); got != "store-tok" {
		t.Fatalf("OSS: %q", got)
	}
	SetOrgCredentials(orgOnly{"gh/token": "account-tok"})
	if got := tokenFor(context.Background(), "gh", ""); got != "account-tok" {
		t.Fatalf("org account: %q", got)
	}
	if got := envSecretFor(context.Background(), "pg", "PGPASSWORD", ""); got != "" {
		t.Fatalf("an unbound slot must not fall back to the legacy store entry, got %q", got)
	}
	t.Setenv("GH_TOKEN_ENV", "env-tok")
	if got := tokenFor(context.Background(), "gh", "GH_TOKEN_ENV"); got != "env-tok" {
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
	t.Cleanup(func() { SetOrgCredentials(nil) })

	p := ctxProbe{got: make(chan context.Context, 1)}
	SetOrgCredentials(p)
	ctx := context.WithValue(context.Background(), probeKey{}, "trace-1")
	if got := tokenFor(ctx, "gh", ""); got != "account-tok" {
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
	SetOrgCredentials(hung)
	cctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan string, 1)
	go func() { done <- envSecretFor(cctx, "pg", "PGPASSWORD", "") }()
	select {
	case got := <-done:
		if got != "" {
			t.Fatalf("a timed-out ask must yield no credential, got %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a hung provider stalled the credential lookup past the caller's deadline")
	}
}
