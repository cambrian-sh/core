package identity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// Owner decision 2026-11-14, third round
// (T-20261106-entra-sign-in-and-entra-scim-provisioning): an Entra sign-in whose
// pairwise `sub` is unknown is linked, by its `oid`, to the person an Entra SCIM
// push provisioned (externalId = object id) under the same issuer/tenant.
// Ambiguity, another tenant or a non-SCIM person means no auto-link.

const entraTenant = "72f988bf-86f1-41af-91ab-2d7cd011db47"

// entraIdP serves discovery and JWKS under an Entra-shaped issuer path
// (<server>/<tid>/v2.0), so the verifier's tenant check sees a real tenant segment.
func entraIdP(t *testing.T, tid string) (issuer string, f fixture) {
	t.Helper()
	f = newFixture(t) // keys, registry, clock
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/"+tid+"/v2.0/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": srv.URL + "/" + tid + "/v2.0", "jwks_uri": srv.URL + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{rsaJWK("a", f.kA, "RS256")}})
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL + "/" + tid + "/v2.0", f
}

func entraVerifier(t *testing.T, f fixture, issuer string, policy FirstLoginPolicy) *OIDCVerifier {
	t.Helper()
	v, err := NewOIDCVerifier(OIDCConfig{Issuer: issuer, ClientID: "console", FirstLogin: policy,
		DefaultRoles: []string{"viewer"}, Now: f.now}, f.reg)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func entraClaims(f fixture, issuer, sub, oid, tid string) map[string]any {
	c := map[string]any{"iss": issuer, "sub": sub, "aud": "console",
		"exp": f.now().Add(10 * time.Minute).Unix(), "iat": f.now().Unix(), "name": "Alice", "email": "alice@example.org"}
	if oid != "" {
		c["oid"] = oid
	}
	if tid != "" {
		c["tid"] = tid
	}
	return c
}

// provisionLikeOrgdir does to the registry exactly what premium
// orgdir.Directory.EnsurePerson does for an Entra SCIM user: a SCIM-sourced person
// keyed by the externalId, linked under the connection's SCIM issuer AND under the
// connection's OIDC issuer (cambrian-premium/orgdir/directory.go EnsurePerson).
func provisionLikeOrgdir(t *testing.T, reg domain.PrincipalRegistry, connID, oidcIssuer, externalID string) string {
	t.Helper()
	ctx := context.Background()
	scimIssuer := "scim:" + connID
	id := domain.PersonIDFor(string(domain.SourceSCIM), scimIssuer, externalID)
	if _, _, err := reg.EnsurePrincipal(ctx, domain.Principal{ID: id, Kind: domain.PrincipalUser,
		DisplayName: "Alice", Status: domain.PrincipalActive, Source: domain.SourceSCIM, ExternalID: externalID,
		Roles: []string{"operator"}}); err != nil {
		t.Fatal(err)
	}
	for _, iss := range []string{scimIssuer, oidcIssuer} {
		if err := reg.LinkIdentity(ctx, domain.ExternalIdentity{Issuer: iss, Subject: externalID, PrincipalID: id}); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func countPersons(t *testing.T, reg domain.PrincipalRegistry) int {
	t.Helper()
	all, err := reg.ListPrincipals(context.Background(), domain.PrincipalFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return len(all)
}

// End to end: SCIM provisions the person, then the person signs in with a pairwise
// sub — one person, linked once, the same person on every later sign-in, with the
// provisioned roles, under BOTH first-login policies.
func TestOIDC_EntraProvisionThenSignInIsOnePerson(t *testing.T) {
	for _, policy := range []FirstLoginPolicy{FirstLoginRequireProvisioned, FirstLoginAutoCreate} {
		t.Run(string(policy), func(t *testing.T) {
			issuer, f := entraIdP(t, entraTenant)
			const oid = "00000000-0000-0000-0000-0000000000a1"
			want := provisionLikeOrgdir(t, f.reg, "entra", issuer, oid)
			v := entraVerifier(t, f, issuer, policy)

			tok := signJWT(t, "RS256", "a", f.kA, entraClaims(f, issuer, "pairwise-sub-xyz", oid, entraTenant))
			id := mustResolve(t, v, tok)
			if id.Principal.ID != want {
				t.Fatalf("sign-in resolved to %q, want the provisioned person %q", id.Principal.ID, want)
			}
			if got := countPersons(t, f.reg); got != 1 {
				t.Fatalf("registry holds %d persons after provision + sign-in, want 1", got)
			}
			// The link is recorded: the pairwise sub now resolves directly.
			if p, err := f.reg.ResolveIdentity(context.Background(), issuer, "pairwise-sub-xyz"); err != nil || p.ID != want {
				t.Fatalf("(iss, sub) not linked to the provisioned person: %v %v", p.ID, err)
			}
			if id2 := mustResolve(t, v, tok); id2.Principal != id.Principal {
				t.Fatal("second sign-in reached a different person")
			}
			if len(id.Roles) == 0 || id.Roles[0] != "operator" {
				t.Fatalf("roles = %v, want the provisioned operator role", id.Roles)
			}
		})
	}
}

func TestOIDC_OIDLink_NoAutoLinkCases(t *testing.T) {
	const oid = "00000000-0000-0000-0000-0000000000b2"
	cases := map[string]func(t *testing.T, issuer string, f fixture) map[string]any{
		"no oid claim": func(t *testing.T, issuer string, f fixture) map[string]any {
			provisionLikeOrgdir(t, f.reg, "entra", issuer, oid)
			return entraClaims(f, issuer, "pairwise", "", entraTenant)
		},
		"no tid claim": func(t *testing.T, issuer string, f fixture) map[string]any {
			provisionLikeOrgdir(t, f.reg, "entra", issuer, oid)
			return entraClaims(f, issuer, "pairwise", oid, "")
		},
		"another tenant": func(t *testing.T, issuer string, f fixture) map[string]any {
			provisionLikeOrgdir(t, f.reg, "entra", issuer, oid)
			return entraClaims(f, issuer, "pairwise", oid, "11111111-2222-3333-4444-555555555555")
		},
		"oid links to a person that is not SCIM-provisioned": func(t *testing.T, issuer string, f fixture) map[string]any {
			ctx := context.Background()
			if _, _, err := f.reg.EnsurePrincipal(ctx, domain.Principal{ID: "bob", Source: domain.SourceLocal, ExternalID: oid}); err != nil {
				t.Fatal(err)
			}
			if err := f.reg.LinkIdentity(ctx, domain.ExternalIdentity{Issuer: issuer, Subject: oid, PrincipalID: "bob"}); err != nil {
				t.Fatal(err)
			}
			return entraClaims(f, issuer, "pairwise", oid, entraTenant)
		},
		"ambiguous: two provisioned persons carry the externalId": func(t *testing.T, issuer string, f fixture) map[string]any {
			provisionLikeOrgdir(t, f.reg, "entra", issuer, oid)
			// A second directory provisioned the same externalId without the OIDC link.
			if _, _, err := f.reg.EnsurePrincipal(context.Background(), domain.Principal{
				ID: "scim-other", Source: domain.SourceSCIM, ExternalID: oid}); err != nil {
				t.Fatal(err)
			}
			return entraClaims(f, issuer, "pairwise", oid, entraTenant)
		},
		"oid provisioned under another issuer only": func(t *testing.T, issuer string, f fixture) map[string]any {
			provisionLikeOrgdir(t, f.reg, "entra", "https://login.microsoftonline.com/"+entraTenant+"/v2.0-other", oid)
			return entraClaims(f, issuer, "pairwise", oid, entraTenant)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			issuer, f := entraIdP(t, entraTenant)
			claims := setup(t, issuer, f)
			before := countPersons(t, f.reg)
			strict := entraVerifier(t, f, issuer, FirstLoginRequireProvisioned)
			mustUnauth(t, strict, signJWT(t, "RS256", "a", f.kA, claims))
			if _, err := f.reg.ResolveIdentity(context.Background(), issuer, "pairwise"); err == nil {
				t.Fatal("the pairwise sub was linked although the case must not auto-link")
			}
			if got := countPersons(t, f.reg); got != before {
				t.Fatalf("persons %d → %d: a refused sign-in must not create anyone", before, got)
			}
		})
	}
}

// A non-Entra issuer (no tenant GUID in its path) never joins by oid, even when a
// provisioned person's externalId equals the token's oid claim.
func TestOIDC_OIDLink_NotForNonEntraIssuers(t *testing.T) {
	f := newFixture(t)
	issuer := f.idp.srv.URL
	const oid = "00000000-0000-0000-0000-0000000000c3"
	provisionLikeOrgdir(t, f.reg, "kc", issuer, oid)
	v := f.verifier(t, func(c *OIDCConfig) { c.FirstLogin = FirstLoginRequireProvisioned })
	c := f.claims("kc-sub")
	c["oid"], c["tid"] = oid, entraTenant
	mustUnauth(t, v, signJWT(t, "RS256", "a", f.kA, c))
}

func TestIsGUIDAndIssuerCarriesTenant(t *testing.T) {
	if !isGUID(entraTenant) || isGUID("realms") || isGUID("72f988bf86f141af91ab2d7cd011db47") {
		t.Fatal("isGUID")
	}
	if !issuerCarriesTenant("https://login.microsoftonline.com/"+entraTenant+"/v2.0", entraTenant) ||
		!issuerCarriesTenant("https://sts.windows.net/"+entraTenant+"/", entraTenant) ||
		issuerCarriesTenant("https://kc.example/realms/acme", entraTenant) {
		t.Fatal("issuerCarriesTenant")
	}
}
