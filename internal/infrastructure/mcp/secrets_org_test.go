package mcp

import (
	"context"
	"testing"

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

	if got := tokenFor("gh", ""); got != "store-tok" {
		t.Fatalf("OSS: %q", got)
	}
	SetOrgCredentials(orgOnly{"gh/token": "account-tok"})
	if got := tokenFor("gh", ""); got != "account-tok" {
		t.Fatalf("org account: %q", got)
	}
	if got := envSecretFor("pg", "PGPASSWORD", ""); got != "" {
		t.Fatalf("an unbound slot must not fall back to the legacy store entry, got %q", got)
	}
	t.Setenv("GH_TOKEN_ENV", "env-tok")
	if got := tokenFor("gh", "GH_TOKEN_ENV"); got != "env-tok" {
		t.Fatalf("the environment still wins: %q", got)
	}
}
