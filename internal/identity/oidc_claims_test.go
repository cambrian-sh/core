package identity

import (
	"encoding/json"
	"testing"
)

// ADR-0142 stage 2 (JIT mapping at login): a verified token's claims ride the
// Identity so a premium wrapper can apply mapping rules from them. A local
// session carries none.
func TestOIDC_IdentityCarriesVerifiedClaims(t *testing.T) {
	f := newFixture(t)
	v := f.verifier(t, nil)
	c := f.claims("s1")
	c["groups"] = []string{"/finance/ap"}
	c["department"] = "Finance"
	id := mustResolve(t, v, signJWT(t, "RS256", "a", f.kA, c))
	var groups []string
	if err := json.Unmarshal(id.Claims["groups"], &groups); err != nil || len(groups) != 1 || groups[0] != "/finance/ap" {
		t.Fatalf("groups claim = %s (%v)", id.Claims["groups"], err)
	}
	var dept string
	if err := json.Unmarshal(id.Claims["department"], &dept); err != nil || dept != "Finance" {
		t.Fatalf("department claim = %s", id.Claims["department"])
	}
	// A token that does not verify never yields claims (it yields nothing).
	_, other := rsaKeys(t)
	mustUnauth(t, v, signJWT(t, "RS256", "a", other, c))
}
