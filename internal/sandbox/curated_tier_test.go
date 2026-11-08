package sandbox

import "testing"

// Owner decision 2026-11-06 (T-20261106-mark-code-sandbox-vetted-add-a-curated-catalog):
// a "curated" catalog entry (reviewed + pinned community server, e.g. code-sandbox)
// launches at the vetted tier by default, while plain "community" stays third-party.
func TestDefaultPolicy_CuratedMapsToVetted(t *testing.T) {
	p := DefaultPolicy()
	if got := p.CatalogTiers["curated"]; got != TierVetted {
		t.Fatalf("curated -> %q, want %q", got, TierVetted)
	}
	if got := p.CatalogTiers["community"]; got != TierThirdParty {
		t.Fatalf("community -> %q, want %q", got, TierThirdParty)
	}
}
