package identity

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ── test IdP ────────────────────────────────────────────────────────────────

var (
	rsaKeysOnce sync.Once
	rsaKeyA     *rsa.PrivateKey
	rsaKeyB     *rsa.PrivateKey
)

func rsaKeys(t *testing.T) (*rsa.PrivateKey, *rsa.PrivateKey) {
	t.Helper()
	rsaKeysOnce.Do(func() {
		var err error
		if rsaKeyA, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
		if rsaKeyB, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
	})
	return rsaKeyA, rsaKeyB
}

type testIdP struct {
	srv          *httptest.Server
	mu           sync.Mutex
	issuerInDoc  string // "" ⇒ the server URL
	keys         []map[string]string
	down         bool
	jwksFetches  atomic.Int32
	discoFetches atomic.Int32
}

func newTestIdP(t *testing.T) *testIdP {
	t.Helper()
	idp := &testIdP{}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		idp.discoFetches.Add(1)
		idp.mu.Lock()
		defer idp.mu.Unlock()
		if idp.down {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		iss := idp.issuerInDoc
		if iss == "" {
			iss = idp.srv.URL
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": iss, "jwks_uri": idp.srv.URL + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		idp.jwksFetches.Add(1)
		idp.mu.Lock()
		defer idp.mu.Unlock()
		if idp.down {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": idp.keys})
	})
	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)
	return idp
}

func (idp *testIdP) setKeys(keys ...map[string]string) {
	idp.mu.Lock()
	idp.keys = keys
	idp.mu.Unlock()
}

func (idp *testIdP) setDown(d bool) {
	idp.mu.Lock()
	idp.down = d
	idp.mu.Unlock()
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func rsaJWK(kid string, k *rsa.PrivateKey, alg string) map[string]string {
	m := map[string]string{"kty": "RSA", "kid": kid, "use": "sig",
		"n": b64(k.N.Bytes()), "e": b64(big.NewInt(int64(k.E)).Bytes())}
	if alg != "" {
		m["alg"] = alg
	}
	return m
}

func ecJWK(kid string, k *ecdsa.PrivateKey) map[string]string {
	n := (k.Curve.Params().BitSize + 7) / 8
	return map[string]string{"kty": "EC", "kid": kid, "crv": k.Curve.Params().Name,
		"x": b64(k.X.FillBytes(make([]byte, n))), "y": b64(k.Y.FillBytes(make([]byte, n)))}
}

func signJWT(t *testing.T, alg, kid string, key any, claims map[string]any) string {
	t.Helper()
	hdr := map[string]any{"alg": alg, "typ": "JWT"}
	if kid != "" {
		hdr["kid"] = kid
	}
	hb, _ := json.Marshal(hdr)
	cb, _ := json.Marshal(claims)
	input := b64(hb) + "." + b64(cb)
	digest := sha256.Sum256([]byte(input))
	var sig []byte
	var err error
	switch k := key.(type) {
	case *rsa.PrivateKey:
		if alg == "PS256" {
			sig, err = rsa.SignPSS(rand.Reader, k, crypto.SHA256, digest[:], &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
		} else {
			sig, err = rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, digest[:])
		}
	case *ecdsa.PrivateKey:
		var r, s *big.Int
		r, s, err = ecdsa.Sign(rand.Reader, k, digest[:])
		n := (k.Curve.Params().BitSize + 7) / 8
		sig = append(r.FillBytes(make([]byte, n)), s.FillBytes(make([]byte, n))...)
	case []byte:
		m := hmac.New(sha256.New, k)
		m.Write([]byte(input))
		sig = m.Sum(nil)
	case nil:
		sig = nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + b64(sig)
}

var t0 = time.Unix(1_800_000_000, 0)

type fixture struct {
	idp   *testIdP
	reg   *MemRegistry
	clock *time.Time
	mu    *sync.Mutex
	kA    *rsa.PrivateKey
}

func (f fixture) now() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return *f.clock }
func (f fixture) advance(d time.Duration) {
	f.mu.Lock()
	*f.clock = f.clock.Add(d)
	f.mu.Unlock()
}

func newFixture(t *testing.T) fixture {
	kA, _ := rsaKeys(t)
	idp := newTestIdP(t)
	idp.setKeys(rsaJWK("a", kA, "RS256"))
	c := t0
	return fixture{idp: idp, reg: NewMemRegistry(), clock: &c, mu: &sync.Mutex{}, kA: kA}
}

func (f fixture) verifier(t *testing.T, mut func(*OIDCConfig)) *OIDCVerifier {
	t.Helper()
	cfg := OIDCConfig{Issuer: f.idp.srv.URL, ClientID: "console", FirstLogin: FirstLoginAutoCreate,
		DefaultRoles: []string{"viewer"}, Now: f.now}
	if mut != nil {
		mut(&cfg)
	}
	v, err := NewOIDCVerifier(cfg, f.reg)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (f fixture) claims(sub string) map[string]any {
	return map[string]any{"iss": f.idp.srv.URL, "sub": sub, "aud": "console",
		"exp": f.now().Add(10 * time.Minute).Unix(), "iat": f.now().Unix(), "name": "Alice", "email": "alice@example.org"}
}

func mustUnauth(t *testing.T, v *OIDCVerifier, tok string) {
	t.Helper()
	if _, err := v.Resolve(context.Background(), tok); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("want ErrUnauthenticated, got %v", err)
	}
}

func mustResolve(t *testing.T, v *OIDCVerifier, tok string) domain.Identity {
	t.Helper()
	id, err := v.Resolve(context.Background(), tok)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return id
}

// ── tests ───────────────────────────────────────────────────────────────────

func TestOIDC_ValidRS256AndES256(t *testing.T) {
	f := newFixture(t)
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	f.idp.setKeys(rsaJWK("a", f.kA, "RS256"), ecJWK("e", ec), rsaJWK("p", f.kA, "PS256"))
	v := f.verifier(t, nil)

	id := mustResolve(t, v, signJWT(t, "RS256", "a", f.kA, f.claims("s1")))
	want := domain.PersonIDFor("oidc", f.idp.srv.URL, "s1")
	if id.Principal != (domain.PrincipalRef{ID: want, Kind: domain.PrincipalUser}) || id.Issuer != f.idp.srv.URL {
		t.Fatalf("identity = %+v", id)
	}
	if !id.ExpiresAt.Equal(f.now().Add(10 * time.Minute)) {
		t.Fatalf("expires %v", id.ExpiresAt)
	}
	mustResolve(t, v, signJWT(t, "ES256", "e", ec, f.claims("s1")))
	mustResolve(t, v, signJWT(t, "PS256", "p", f.kA, f.claims("s1")))
	// RS256 token presented against the PS256-labelled JWK: alg mismatch.
	mustUnauth(t, v, signJWT(t, "RS256", "p", f.kA, f.claims("s1")))
	// ES256 header against an RSA key: kty mismatch.
	mustUnauth(t, v, signJWT(t, "ES256", "a", ec, f.claims("s1")))
}

func TestOIDC_ClaimChecks(t *testing.T) {
	f := newFixture(t)
	v := f.verifier(t, nil)
	cases := map[string]func(c map[string]any){
		"wrong audience":   func(c map[string]any) { c["aud"] = "other-app" },
		"aud array miss":   func(c map[string]any) { c["aud"] = []string{"x", "y"} },
		"wrong issuer":     func(c map[string]any) { c["iss"] = "https://evil.example" },
		"expired":          func(c map[string]any) { c["exp"] = f.now().Add(-2 * time.Minute).Unix() },
		"exp missing":      func(c map[string]any) { delete(c, "exp") },
		"nbf future":       func(c map[string]any) { c["nbf"] = f.now().Add(5 * time.Minute).Unix() },
		"iat future":       func(c map[string]any) { c["iat"] = f.now().Add(5 * time.Minute).Unix() },
		"sub missing":      func(c map[string]any) { delete(c, "sub") },
		"exp not a number": func(c map[string]any) { c["exp"] = "tomorrow" },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			c := f.claims("s1")
			mut(c)
			mustUnauth(t, v, signJWT(t, "RS256", "a", f.kA, c))
		})
	}
	t.Run("aud array hit", func(t *testing.T) {
		c := f.claims("s1")
		c["aud"] = []string{"x", "console"}
		mustResolve(t, v, signJWT(t, "RS256", "a", f.kA, c))
	})
}

func TestOIDC_ClockSkewTolerance(t *testing.T) {
	f := newFixture(t)
	v := f.verifier(t, func(c *OIDCConfig) { c.ClockSkew = 60 * time.Second })
	c := f.claims("s1")
	c["exp"] = f.now().Add(-30 * time.Second).Unix() // expired, but inside skew
	c["nbf"] = f.now().Add(30 * time.Second).Unix()  // not yet valid, but inside skew
	c["iat"] = f.now().Add(30 * time.Second).Unix()
	mustResolve(t, v, signJWT(t, "RS256", "a", f.kA, c))
	c["exp"] = f.now().Add(-90 * time.Second).Unix()
	mustUnauth(t, v, signJWT(t, "RS256", "a", f.kA, c))
}

func TestOIDC_RefusesNoneHSAndTamper(t *testing.T) {
	f := newFixture(t)
	v := f.verifier(t, nil)
	mustUnauth(t, v, signJWT(t, "none", "a", nil, f.claims("s1")))
	// An "alg: none" token with an empty signature segment is not even JWT-shaped.
	tok := signJWT(t, "none", "a", nil, f.claims("s1"))
	if LooksLikeJWT(tok) {
		t.Fatal("alg none token with empty signature looks like a JWT")
	}
	mustUnauth(t, v, signJWT(t, "none", "a", []byte("x"), f.claims("s1")))
	// HS256 keyed with the public modulus (classic key-confusion attack).
	mustUnauth(t, v, signJWT(t, "HS256", "a", f.kA.N.Bytes(), f.claims("s1")))

	good := signJWT(t, "RS256", "a", f.kA, f.claims("s1"))
	parts := strings.Split(good, ".")
	c := f.claims("s1")
	c["sub"] = "admin"
	cb, _ := json.Marshal(c)
	mustUnauth(t, v, parts[0]+"."+b64(cb)+"."+parts[2])
	mustUnauth(t, v, "not-a-jwt")
}

func TestOIDC_DiscoveryIssuerMismatch(t *testing.T) {
	f := newFixture(t)
	f.idp.issuerInDoc = "https://someone-else.example"
	v := f.verifier(t, nil)
	mustUnauth(t, v, signJWT(t, "RS256", "a", f.kA, f.claims("s1")))
	if f.idp.jwksFetches.Load() != 0 {
		t.Fatal("JWKS fetched despite a discovery issuer mismatch")
	}
}

func TestOIDC_KeyRotationRefetchIsRateLimited(t *testing.T) {
	f := newFixture(t)
	_, kB := rsaKeys(t)
	v := f.verifier(t, func(c *OIDCConfig) { c.MinRefetch = 30 * time.Second })
	mustResolve(t, v, signJWT(t, "RS256", "a", f.kA, f.claims("s1")))
	if n := f.idp.jwksFetches.Load(); n != 1 {
		t.Fatalf("jwks fetches = %d, want 1", n)
	}
	// Rotation: the IdP now publishes b. Within MinRefetch of the last fetch the
	// unknown kid must not refetch.
	f.idp.setKeys(rsaJWK("b", kB, "RS256"))
	mustUnauth(t, v, signJWT(t, "RS256", "b", kB, f.claims("s1")))
	if n := f.idp.jwksFetches.Load(); n != 1 {
		t.Fatalf("refetched within MinRefetch: %d", n)
	}
	f.advance(31 * time.Second)
	mustResolve(t, v, signJWT(t, "RS256", "b", kB, f.claims("s1")))
	if n := f.idp.jwksFetches.Load(); n != 2 {
		t.Fatalf("jwks fetches = %d, want 2", n)
	}
	// A second unknown kid right after: refused with no further fetch.
	mustUnauth(t, v, signJWT(t, "RS256", "c", kB, f.claims("s1")))
	if n := f.idp.jwksFetches.Load(); n != 2 {
		t.Fatalf("second unknown kid refetched: %d", n)
	}
	if n := f.idp.discoFetches.Load(); n != 1 {
		t.Fatalf("discovery fetches = %d, want 1 (cached)", n)
	}
}

func TestOIDC_JWKSCacheRefresh(t *testing.T) {
	f := newFixture(t)
	v := f.verifier(t, func(c *OIDCConfig) { c.JWKSRefresh = time.Hour })
	tok := func() string { return signJWT(t, "RS256", "a", f.kA, f.claims("s1")) }
	mustResolve(t, v, tok())
	f.advance(30 * time.Minute)
	mustResolve(t, v, tok())
	if n := f.idp.jwksFetches.Load(); n != 1 {
		t.Fatalf("fetches = %d before expiry", n)
	}
	f.advance(31 * time.Minute)
	mustResolve(t, v, tok())
	if n := f.idp.jwksFetches.Load(); n != 2 {
		t.Fatalf("fetches = %d after JWKSRefresh, want 2", n)
	}
	// A refresh that fails keeps the cached set in use.
	f.idp.setDown(true)
	f.advance(2 * time.Hour)
	mustResolve(t, v, tok())
}

func TestOIDC_FirstLoginAutoCreate(t *testing.T) {
	f := newFixture(t)
	v := f.verifier(t, nil)
	id1 := mustResolve(t, v, signJWT(t, "RS256", "a", f.kA, f.claims("s1")))
	want := domain.PersonIDFor("oidc", f.idp.srv.URL, "s1")
	if !strings.HasPrefix(want, "oidc-") || id1.Principal.ID != want {
		t.Fatalf("id = %q, want %q", id1.Principal.ID, want)
	}
	p, err := f.reg.GetPrincipal(context.Background(), want)
	if err != nil {
		t.Fatal(err)
	}
	if p.Source != domain.SourceOIDC || p.ExternalID != "s1" || p.DisplayName != "Alice" ||
		p.Email != "alice@example.org" || !reflect.DeepEqual(p.Roles, []string{"viewer"}) {
		t.Fatalf("created person = %+v", p)
	}
	id2 := mustResolve(t, v, signJWT(t, "RS256", "a", f.kA, f.claims("s1")))
	if id2.Principal != id1.Principal {
		t.Fatal("second login created a different person")
	}
	all, _ := f.reg.ListPrincipals(context.Background(), domain.PrincipalFilter{})
	if len(all) != 1 {
		t.Fatalf("registry holds %d persons, want 1", len(all))
	}
}

func TestOIDC_RequireProvisioned(t *testing.T) {
	f := newFixture(t)
	v := f.verifier(t, func(c *OIDCConfig) { c.FirstLogin = FirstLoginRequireProvisioned })
	mustUnauth(t, v, signJWT(t, "RS256", "a", f.kA, f.claims("s1")))
	ctx := context.Background()
	if _, _, err := f.reg.EnsurePrincipal(ctx, domain.Principal{ID: "alice", Roles: []string{"operator"}}); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.LinkIdentity(ctx, domain.ExternalIdentity{Issuer: f.idp.srv.URL, Subject: "s1", PrincipalID: "alice"}); err != nil {
		t.Fatal(err)
	}
	id := mustResolve(t, v, signJWT(t, "RS256", "a", f.kA, f.claims("s1")))
	if id.Principal.ID != "alice" || !reflect.DeepEqual(id.Roles, []string{"operator"}) {
		t.Fatalf("identity = %+v", id)
	}
	// Default policy (unset) is require_provisioned.
	v2 := f.verifier(t, func(c *OIDCConfig) { c.FirstLogin = "" })
	mustUnauth(t, v2, signJWT(t, "RS256", "a", f.kA, f.claims("s2")))
}

func TestOIDC_SuspendedPersonRefused(t *testing.T) {
	f := newFixture(t)
	v := f.verifier(t, nil)
	id := mustResolve(t, v, signJWT(t, "RS256", "a", f.kA, f.claims("s1")))
	if err := f.reg.SetStatus(context.Background(), id.Principal.ID, domain.PrincipalSuspended); err != nil {
		t.Fatal(err)
	}
	mustUnauth(t, v, signJWT(t, "RS256", "a", f.kA, f.claims("s1")))
}

func TestOIDC_RoleMapping(t *testing.T) {
	f := newFixture(t)
	v := f.verifier(t, func(c *OIDCConfig) {
		c.RolesClaim = "groups"
		c.RoleMap = map[string][]string{"cambrian-admins": {"operator", "admin"}, "staff": {"viewer"}}
	})
	c := f.claims("s1")
	c["groups"] = "cambrian-admins"
	id := mustResolve(t, v, signJWT(t, "RS256", "a", f.kA, c))
	if !reflect.DeepEqual(id.Roles, []string{"admin", "operator", "viewer"}) {
		t.Fatalf("string claim roles = %v", id.Roles)
	}
	c = f.claims("s2")
	c["groups"] = []string{"staff", "unmapped-group"}
	id = mustResolve(t, v, signJWT(t, "RS256", "a", f.kA, c))
	if !reflect.DeepEqual(id.Roles, []string{"viewer"}) {
		t.Fatalf("array claim roles = %v (unmapped values must grant nothing)", id.Roles)
	}
}

func TestOIDC_EmailNeverMapsAPerson(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, _, err := f.reg.EnsurePrincipal(ctx, domain.Principal{ID: "alice", Email: "alice@example.org"}); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.LinkIdentity(ctx, domain.ExternalIdentity{Issuer: f.idp.srv.URL, Subject: "s1", Email: "alice@example.org", PrincipalID: "alice"}); err != nil {
		t.Fatal(err)
	}
	strict := f.verifier(t, func(c *OIDCConfig) { c.FirstLogin = FirstLoginRequireProvisioned })
	mustUnauth(t, strict, signJWT(t, "RS256", "a", f.kA, f.claims("s-new-holder")))
	auto := f.verifier(t, nil)
	id := mustResolve(t, auto, signJWT(t, "RS256", "a", f.kA, f.claims("s-new-holder")))
	if id.Principal.ID == "alice" {
		t.Fatal("same email, different subject resolved to the existing person")
	}
}

func TestOIDC_IdPDownThenUp(t *testing.T) {
	f := newFixture(t)
	f.idp.setDown(true)
	v := f.verifier(t, func(c *OIDCConfig) { c.MinRefetch = 30 * time.Second })
	tok := func() string { return signJWT(t, "RS256", "a", f.kA, f.claims("s1")) }
	mustUnauth(t, v, tok())
	f.idp.setDown(false)
	mustUnauth(t, v, tok()) // rate-limited: no new attempt yet
	f.advance(31 * time.Second)
	mustResolve(t, v, tok())
}

func TestOIDC_Metadata_Revoke_LooksLikeJWT(t *testing.T) {
	f := newFixture(t)
	v := f.verifier(t, nil)
	if md := v.AuthMetadata(); md.OIDCIssuer != f.idp.srv.URL || md.OIDCClientID != "console" || md.PasswordLogin {
		t.Fatalf("metadata = %+v", md)
	}
	if err := v.Revoke(context.Background(), "anything"); err != nil {
		t.Fatal(err)
	}
	if !LooksLikeJWT(signJWT(t, "RS256", "a", f.kA, f.claims("s1"))) {
		t.Fatal("real JWT not recognised")
	}
	for _, s := range []string{"", "abc", "a.b", "a.b.c.d", "deadbeefcafebabe", "YQ.YQ.YQ", "e30.e30.a+b"} {
		if LooksLikeJWT(s) {
			t.Fatalf("%q looks like a JWT", s)
		}
	}
}

func TestOIDC_ConfigValidation(t *testing.T) {
	reg := NewMemRegistry()
	bad := map[string]OIDCConfig{
		"no issuer":           {ClientID: "c"},
		"http non-loopback":   {Issuer: "http://idp.example", ClientID: "c"},
		"relative issuer":     {Issuer: "idp.example", ClientID: "c"},
		"no client/audiences": {Issuer: "https://idp.example"},
		"unknown first login": {Issuer: "https://idp.example", ClientID: "c", FirstLogin: "sometimes"},
		"negative skew":       {Issuer: "https://idp.example", ClientID: "c", ClockSkew: -time.Second},
	}
	for name, cfg := range bad {
		if _, err := NewOIDCVerifier(cfg, reg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := NewOIDCVerifier(OIDCConfig{Issuer: "https://idp.example", ClientID: "c"}, nil); err == nil {
		t.Error("nil registry accepted")
	}
	for _, iss := range []string{"https://idp.example/realms/x", "http://localhost:8080", "http://127.0.0.1:9", "http://[::1]:9"} {
		if _, err := NewOIDCVerifier(OIDCConfig{Issuer: iss, Audiences: []string{"api"}}, reg); err != nil {
			t.Errorf("%s rejected: %v", iss, err)
		}
	}
}

// ── Keycloak-shaped IdP (ADR-0142 D2: Keycloak is the bundled IdP) ──────────

// keycloakIdP serves a realm the way Keycloak does: issuer
// <base>/realms/<realm>, discovery under it, keys at protocol/openid-connect/certs.
func keycloakIdP(t *testing.T, realm string, keys func() []map[string]string) (issuer string) {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	prefix := "/realms/" + realm
	mux.HandleFunc(prefix+"/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		iss := srv.URL + prefix
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                iss,
			"authorization_endpoint":                iss + "/protocol/openid-connect/auth",
			"token_endpoint":                        iss + "/protocol/openid-connect/token",
			"userinfo_endpoint":                     iss + "/protocol/openid-connect/userinfo",
			"end_session_endpoint":                  iss + "/protocol/openid-connect/logout",
			"device_authorization_endpoint":         iss + "/protocol/openid-connect/auth/device",
			"jwks_uri":                              iss + "/protocol/openid-connect/certs",
			"grant_types_supported":                 []string{"authorization_code", "refresh_token", "urn:ietf:params:oauth:grant-type:device_code"},
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public", "pairwise"},
			"id_token_signing_alg_values_supported": []string{"RS256", "ES256", "PS256"},
			"code_challenge_methods_supported":      []string{"plain", "S256"},
			"scopes_supported":                      []string{"openid", "profile", "email", "roles"},
		})
	})
	mux.HandleFunc(prefix+"/protocol/openid-connect/certs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys()})
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL + prefix
}

func keycloakClaims(iss, sub string, now time.Time) map[string]any {
	return map[string]any{
		"exp": now.Add(5 * time.Minute).Unix(), "iat": now.Unix(), "auth_time": now.Unix(),
		"jti": "6f1c0f2e-1", "iss": iss, "sub": sub, "typ": "Bearer",
		"aud": []string{"cambrian-console", "account"}, "azp": "cambrian-console",
		"session_state": "b6d4", "scope": "openid profile email",
		"email_verified": true, "preferred_username": "ayse", "email": "ayse@agency.example",
		"groups":       []string{"/cambrian-admins", "/staff"},
		"realm_access": map[string]any{"roles": []string{"offline_access", "uma_authorization"}},
	}
}

func TestOIDC_KeycloakRealm(t *testing.T) {
	kA, _ := rsaKeys(t)
	iss := keycloakIdP(t, "cambrian", func() []map[string]string {
		// Keycloak publishes an RS256 signing key and an RSA-OAEP encryption key.
		enc := rsaJWK("enc-1", kA, "RSA-OAEP")
		enc["use"] = "enc"
		return []map[string]string{rsaJWK("kc-sig-1", kA, "RS256"), enc}
	})
	now := time.Now()
	reg := NewMemRegistry()
	v, err := NewOIDCVerifier(OIDCConfig{
		Issuer: iss, ClientID: "cambrian-console", FirstLogin: FirstLoginAutoCreate,
		RolesClaim: "groups",
		RoleMap:    map[string][]string{"/cambrian-admins": {"operator"}, "/staff": {"viewer"}},
		Now:        func() time.Time { return now },
	}, reg)
	if err != nil {
		t.Fatal(err)
	}
	id := mustResolve(t, v, signJWT(t, "RS256", "kc-sig-1", kA, keycloakClaims(iss, "f81d4fae-7dec", now)))
	if !reflect.DeepEqual(id.Roles, []string{"operator", "viewer"}) {
		t.Fatalf("roles from groups = %v", id.Roles)
	}
	p, err := reg.GetPrincipal(context.Background(), id.Principal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.DisplayName != "ayse" || p.Email != "ayse@agency.example" {
		t.Fatalf("person = %+v (display name falls back to preferred_username)", p)
	}

	// Keycloak's default access token: aud lacks the client id, azp names it.
	// Refused by design: azp is who asked for the token, not who it is for.
	c := keycloakClaims(iss, "f81d4fae-7dec", now)
	c["aud"] = []string{"account", "other-api"}
	mustUnauth(t, v, signJWT(t, "RS256", "kc-sig-1", kA, c))
	delete(c, "aud")
	mustUnauth(t, v, signJWT(t, "RS256", "kc-sig-1", kA, c))
	// The encryption key is never used to verify.
	mustUnauth(t, v, signJWT(t, "RS256", "enc-1", kA, keycloakClaims(iss, "f81d4fae-7dec", now)))
	// A token from another realm on the same host is another issuer.
	c = keycloakClaims(strings.Replace(iss, "/realms/cambrian", "/realms/other", 1), "x", now)
	mustUnauth(t, v, signJWT(t, "RS256", "kc-sig-1", kA, c))
}

// ENG-714 (owner decision 2026-11-14): a Keycloak ID token — typ claim "ID",
// aud = the client id, so it passes the audience check — is not a bearer. The
// access token (typ "Bearer") and a token with no typ claim (other IdPs) still
// resolve; the case of the claim does not matter.
func TestOIDC_KeycloakIDTokenRefusedAsBearer(t *testing.T) {
	kA, _ := rsaKeys(t)
	iss := keycloakIdP(t, "cambrian", func() []map[string]string {
		return []map[string]string{rsaJWK("kc-sig-1", kA, "RS256")}
	})
	now := time.Now()
	v, err := NewOIDCVerifier(OIDCConfig{
		Issuer: iss, ClientID: "cambrian-console", FirstLogin: FirstLoginAutoCreate,
		Now: func() time.Time { return now },
	}, NewMemRegistry())
	if err != nil {
		t.Fatal(err)
	}
	mustResolve(t, v, signJWT(t, "RS256", "kc-sig-1", kA, keycloakClaims(iss, "s1", now)))

	noTyp := keycloakClaims(iss, "s1", now)
	delete(noTyp, "typ")
	mustResolve(t, v, signJWT(t, "RS256", "kc-sig-1", kA, noTyp))

	for _, typ := range []string{"ID", "id", "Id"} {
		idTok := keycloakClaims(iss, "s1", now)
		idTok["typ"] = typ
		idTok["aud"] = "cambrian-console" // an ID token's aud is the client id
		idTok["nonce"] = "n-1"
		mustUnauth(t, v, signJWT(t, "RS256", "kc-sig-1", kA, idTok))
	}
}

func TestOIDC_KeycloakIssuerShapesValidate(t *testing.T) {
	for _, iss := range []string{"https://sso.agency.example/realms/cambrian", "http://127.0.0.1:8080/realms/cambrian", "http://localhost:8080/realms/cambrian"} {
		if _, err := NewOIDCVerifier(OIDCConfig{Issuer: iss, ClientID: "cambrian-console"}, NewMemRegistry()); err != nil {
			t.Errorf("%s: %v", iss, err)
		}
	}
	if _, err := NewOIDCVerifier(OIDCConfig{Issuer: "http://sso.agency.example/realms/cambrian", ClientID: "c"}, NewMemRegistry()); err == nil {
		t.Error("plain-http non-loopback realm accepted")
	}
}

// ── algorithm and key pinning ───────────────────────────────────────────────

func TestOIDC_AllowedAlgorithmsArePinned(t *testing.T) {
	for _, alg := range []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512"} {
		if _, ok := algHash(alg); !ok {
			t.Errorf("%s not allowed", alg)
		}
	}
	for _, alg := range []string{"", "none", "None", "NONE", "HS256", "HS384", "HS512", "RS1", "EdDSA", "RSA-OAEP", "rs256"} {
		if _, ok := algHash(alg); ok {
			t.Errorf("%q allowed", alg)
		}
	}
}

func TestOIDC_KeyHeaderDisagreementRefused(t *testing.T) {
	f := newFixture(t)
	ec256, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	labelled := ecJWK("e-384", ec256)
	labelled["alg"] = "ES384"
	f.idp.setKeys(rsaJWK("a", f.kA, "RS256"), ecJWK("e", ec256), labelled)
	v := f.verifier(t, nil)
	mustResolve(t, v, signJWT(t, "ES256", "e", ec256, f.claims("s1")))
	// JWK alg disagrees with the header alg.
	mustUnauth(t, v, signJWT(t, "ES256", "e-384", ec256, f.claims("s1")))
	mustUnauth(t, v, signJWT(t, "RS384", "a", f.kA, f.claims("s1")))
	// kty disagrees: RSA header against an EC key, EC header against an RSA key.
	mustUnauth(t, v, signJWT(t, "RS256", "e", f.kA, f.claims("s1")))
	mustUnauth(t, v, signJWT(t, "ES256", "a", ec256, f.claims("s1")))
	// crv disagrees: ES384 header against a P-256 key (the ES256 signature reused).
	tok := signJWT(t, "ES256", "e", ec256, f.claims("s1"))
	parts := strings.Split(tok, ".")
	hb, _ := json.Marshal(map[string]string{"alg": "ES384", "kid": "e"})
	mustUnauth(t, v, b64(hb)+"."+parts[1]+"."+parts[2])
	// crit headers are refused.
	hb, _ = json.Marshal(map[string]any{"alg": "ES256", "kid": "e", "crit": []string{"exp"}})
	mustUnauth(t, v, b64(hb)+"."+parts[1]+"."+parts[2])
}

func TestOIDC_InvalidECPointRejected(t *testing.T) {
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	good := ecJWK("e", ec)
	if _, err := parseJWKKey("EC", "", "", good["crv"], good["x"], good["y"]); err != nil {
		t.Fatalf("valid point rejected: %v", err)
	}
	yb, _ := base64.RawURLEncoding.DecodeString(good["y"])
	yb[len(yb)-1] ^= 1 // off the curve
	offCurve := b64(yb)
	zero := b64(make([]byte, 32))
	cases := map[string][3]string{
		"off-curve":        {"P-256", good["x"], offCurve},
		"point at origin":  {"P-256", zero, zero},
		"short coordinate": {"P-256", good["x"][2:], good["y"]},
		"wrong curve name": {"P-384", good["x"], good["y"]},
		"unknown curve":    {"secp256k1", good["x"], good["y"]},
	}
	for name, c := range cases {
		if _, err := parseJWKKey("EC", "", "", c[0], c[1], c[2]); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// End to end: a key set whose only EC key is off-curve verifies nothing.
	f := newFixture(t)
	bad := ecJWK("e", ec)
	bad["y"] = offCurve
	f.idp.setKeys(bad)
	mustUnauth(t, f.verifier(t, nil), signJWT(t, "ES256", "e", ec, f.claims("s1")))
	// Small RSA keys are rejected too.
	small, _ := rsa.GenerateKey(rand.Reader, 1024)
	if _, err := parseJWKKey("RSA", b64(small.N.Bytes()), b64(big.NewInt(int64(small.E)).Bytes()), "", "", ""); err == nil {
		t.Error("1024-bit RSA key accepted")
	}
}
