package app

import "testing"

// T-20261106-testintegration-and-the-catalog-op-do-not: the catalog op carries
// each entry's ADR-0140 credential model, so the console can say "each person
// connects their own account" before install; a shared entry says shared.
func TestIntegrationCatalog_CarriesTheCredentialModel(t *testing.T) {
	r := newIntegrationRig(t, newAuthServer(t).url)
	var perUser, shared bool
	for _, e := range r.m.Catalog() {
		switch e.GetCredentialScope() {
		case "per_user", "either":
			// Scopes may be empty: a GitHub App's user tokens carry the app's
			// permissions and request no OAuth scopes (catalog notes).
			if e.GetProvider() == "" {
				t.Fatalf("%s: a per-user entry must name its account provider: %+v", e.GetId(), e)
			}
			perUser = true
		case "shared":
			if e.GetProvider() != "" {
				t.Fatalf("%s: a shared entry names no account provider", e.GetId())
			}
			shared = true
		default:
			t.Fatalf("%s: credential_scope %q", e.GetId(), e.GetCredentialScope())
		}
	}
	if !perUser || !shared {
		t.Fatalf("the built-in catalog has both per-user and shared entries (per_user=%v shared=%v)", perUser, shared)
	}
}
