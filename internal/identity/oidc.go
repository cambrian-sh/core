package identity

// The single-issuer OIDC verifier (ADR-0142 D1.1).
//
// Cambrian VERIFIES identity and never issues it to people: the console and the
// desktop client run authorization code + PKCE (or the RFC 8628 device grant) at
// the IdP, and the kernel only ever sees the resulting JWT. This file turns that
// JWT into a domain.Identity:
//
//  1. signature against the issuer's JWKS (found through OIDC discovery),
//  2. the registered claims iss / aud / exp / nbf / iat / sub, and the token
//     kind: a Keycloak ID token (typ "ID") is refused as a bearer (ENG-714),
//  3. (iss, sub) → a registry person via PrincipalRegistry.ResolveIdentity —
//     NEVER the email alone, because emails are reassigned (ADR-0142 threat
//     table, impersonation),
//  4. roles = the person's stored roles ∪ a STATIC claim→role map (D1: no
//     expression language, no per-tenant rules engine in core).
//
// JOSE is implemented on the standard library on purpose. Rejected: a JOSE
// module (go-jose, lestrrat-go/jwx, golang-jwt). They are fine libraries, but
// every new module is one more thing an air-gapped install must vendor and a
// procurement review must clear, and the subset a resource server needs —
// verify RS*/PS*/ES* compact JWS against a JWKS — is small and fully covered by
// crypto/rsa and crypto/ecdsa. What we deliberately do NOT implement: JWE,
// symmetric (HS*) algorithms (a shared secret would let any holder of the client
// secret mint tokens), `alg: none`, `jku`/`x5u` header key URLs (an attacker
// chooses the key), and `crit` extensions.

import (
	"context"
	"crypto"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	_ "crypto/sha512" // registers SHA-384/512 behind crypto.Hash for RS384/ES512 etc.
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// FirstLoginPolicy decides what happens when a verified (iss, sub) is linked to
// no registry person yet.
type FirstLoginPolicy string

const (
	// FirstLoginAutoCreate creates the person (id PersonIDFor("oidc", iss, sub),
	// DefaultRoles) and links the identity. For deployments where "the IdP let
	// them in" is the admission decision.
	FirstLoginAutoCreate FirstLoginPolicy = "auto_create"
	// FirstLoginRequireProvisioned refuses until an operator (or the stage-2 SCIM
	// push) has linked the identity. The default: fail closed.
	FirstLoginRequireProvisioned FirstLoginPolicy = "require_provisioned"
)

// OIDCConfig configures the verifier for ONE issuer (ADR-0142 D1: several IdPs
// are federated by the bundled/external IdP, not by the kernel).
type OIDCConfig struct {
	Issuer       string              // exact issuer URL; discovery doc's "issuer" must equal it
	ClientID     string              // advertised to clients (handshake) for PKCE/device grant
	Audiences    []string            // accepted `aud` values; empty ⇒ []string{ClientID}; token must carry at least one
	RolesClaim   string              // claim holding the IdP's roles/groups (string or []string); default "roles"
	RoleMap      map[string][]string // claim value → Cambrian roles (static map, ADR-0142 D1); unmapped values ignored
	DefaultRoles []string            // roles stored on a person created at first login
	FirstLogin   FirstLoginPolicy    // default require_provisioned (fail closed)
	ClockSkew    time.Duration       // default 60s; applied to exp, nbf, iat-in-future
	JWKSRefresh  time.Duration       // max age of the cached key set, default 1h
	MinRefetch   time.Duration       // rate limit on unknown-kid refetches, default 30s
	HTTPClient   *http.Client        // default: 10s timeout client
	Now          func() time.Time
}

const (
	maxTokenBytes    = 16 << 10 // a JWT bigger than this is not an access/ID token
	maxDocumentBytes = 1 << 20  // discovery doc / JWKS body cap
	minRSABits       = 2048
)

// OIDCVerifier implements domain.Authenticator and domain.AuthMetadataSource.
type OIDCVerifier struct {
	cfg OIDCConfig
	reg domain.PrincipalRegistry

	// mu guards the discovery/JWKS cache. Fetches happen under it: that
	// serialises concurrent cache misses into one request instead of a
	// thundering herd against the IdP, and needs no background goroutine.
	mu          sync.Mutex
	jwksURI     string
	keys        map[string]jwk
	fetchedAt   time.Time // last SUCCESSFUL JWKS fetch
	attemptedAt time.Time // last fetch attempt, success or not (rate limit)
}

// jwk is one parsed signing key.
type jwk struct {
	kid string
	alg string // the JWK's own `alg`, "" if unstated
	pub crypto.PublicKey
}

// NewOIDCVerifier validates cfg and returns a verifier. It does NOT contact the
// IdP: an IdP that is down at boot must not fail the kernel (local accounts and
// machine tokens keep working); OIDC tokens are simply refused until discovery
// succeeds.
func NewOIDCVerifier(cfg OIDCConfig, reg domain.PrincipalRegistry) (*OIDCVerifier, error) {
	if reg == nil {
		return nil, errors.New("oidc: principal registry is required")
	}
	cfg.Issuer = strings.TrimSpace(cfg.Issuer)
	if cfg.Issuer == "" {
		return nil, errors.New("oidc: issuer is required")
	}
	u, err := url.Parse(cfg.Issuer)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("oidc: issuer %q is not an absolute URL", cfg.Issuer)
	}
	// Discovery and JWKS over plain http would let anyone on the path swap the
	// signing keys. Loopback is allowed for development and tests only.
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && isLoopbackHost(u.Hostname()):
	default:
		return nil, fmt.Errorf("oidc: issuer %q must use https (http is accepted only for a loopback host)", cfg.Issuer)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("oidc: issuer %q must not carry a query or fragment", cfg.Issuer)
	}
	cfg.ClientID = strings.TrimSpace(cfg.ClientID)
	auds := make([]string, 0, len(cfg.Audiences))
	for _, a := range cfg.Audiences {
		if a = strings.TrimSpace(a); a != "" {
			auds = append(auds, a)
		}
	}
	if len(auds) == 0 && cfg.ClientID != "" {
		auds = []string{cfg.ClientID}
	}
	if len(auds) == 0 {
		return nil, errors.New("oidc: client_id or audiences is required (a token with no audience check is a token minted for any app)")
	}
	cfg.Audiences = auds
	if cfg.RolesClaim == "" {
		cfg.RolesClaim = "roles"
	}
	switch cfg.FirstLogin {
	case "":
		cfg.FirstLogin = FirstLoginRequireProvisioned
	case FirstLoginAutoCreate, FirstLoginRequireProvisioned:
	default:
		return nil, fmt.Errorf("oidc: unknown first_login policy %q (want %q or %q)",
			cfg.FirstLogin, FirstLoginAutoCreate, FirstLoginRequireProvisioned)
	}
	if cfg.ClockSkew < 0 || cfg.JWKSRefresh < 0 || cfg.MinRefetch < 0 {
		return nil, errors.New("oidc: clock_skew, jwks_refresh and min_refetch must not be negative")
	}
	if cfg.ClockSkew == 0 {
		cfg.ClockSkew = 60 * time.Second
	}
	if cfg.JWKSRefresh == 0 {
		cfg.JWKSRefresh = time.Hour
	}
	if cfg.MinRefetch == 0 {
		cfg.MinRefetch = 30 * time.Second
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &OIDCVerifier{cfg: cfg, reg: reg}, nil
}

func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// AuthMetadata advertises the issuer and client id on the handshake so a client
// knows which flow to run (ADR-0142 D1).
func (v *OIDCVerifier) AuthMetadata() domain.AuthMetadata {
	return domain.AuthMetadata{OIDCIssuer: v.cfg.Issuer, OIDCClientID: v.cfg.ClientID}
}

// Revoke is a no-op: only the issuer can revoke its tokens (domain.Authenticator
// contract). Logout at the IdP is the client's job.
func (v *OIDCVerifier) Revoke(context.Context, string) error { return nil }

// LooksLikeJWT reports whether token has the shape of a compact JWS: three
// non-empty base64url segments whose first decodes to a JSON object. The
// authenticator chain uses it to route a bearer to this verifier rather than to
// the opaque-session backend; it proves nothing about validity.
func LooksLikeJWT(token string) bool {
	if len(token) > maxTokenBytes {
		return false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" || strings.IndexFunc(p, notB64URL) >= 0 {
			return false
		}
	}
	hdr, err := base64.RawURLEncoding.DecodeString(parts[0])
	return err == nil && len(hdr) > 0 && hdr[0] == '{'
}

func notB64URL(r rune) bool {
	return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_')
}

// unauth wraps domain.ErrUnauthenticated with an internal reason. The reason is
// for logs and tests; the transport layer answers the caller with the bare
// sentinel (domain.ErrUnauthenticated is deliberately undetailed).
func unauth(format string, a ...any) error {
	return fmt.Errorf("%w: oidc: %s", domain.ErrUnauthenticated, fmt.Sprintf(format, a...))
}

// Resolve verifies token and maps it to a registry person.
func (v *OIDCVerifier) Resolve(ctx context.Context, token string) (domain.Identity, error) {
	id, err := v.resolve(ctx, token)
	if err != nil {
		// Never the token itself: a log line must not be a replayable credential.
		slog.DebugContext(ctx, "oidc: token refused", "issuer", v.cfg.Issuer, "reason", err.Error())
	}
	return id, err
}

func (v *OIDCVerifier) resolve(ctx context.Context, token string) (domain.Identity, error) {
	if !LooksLikeJWT(token) {
		return domain.Identity{}, unauth("not a compact JWS")
	}
	claims, err := v.verifySignature(ctx, token)
	if err != nil {
		return domain.Identity{}, err
	}
	c, err := v.checkClaims(claims)
	if err != nil {
		return domain.Identity{}, err
	}
	person, err := v.person(ctx, c)
	if err != nil {
		return domain.Identity{}, err
	}
	return domain.Identity{
		Principal: person.Ref(),
		Roles:     v.roles(person.Roles, claims[v.cfg.RolesClaim]),
		Issuer:    c.iss,
		ExpiresAt: c.exp,
		Claims:    claims,
	}, nil
}

// ── signature ───────────────────────────────────────────────────────────────

type jwsHeader struct {
	Alg  string          `json:"alg"`
	Kid  string          `json:"kid"`
	Crit json.RawMessage `json:"crit"`
}

// verifySignature checks the JWS and returns the decoded claim set.
func (v *OIDCVerifier) verifySignature(ctx context.Context, token string) (map[string]json.RawMessage, error) {
	parts := strings.Split(token, ".")
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, unauth("header: %v", err)
	}
	var hdr jwsHeader
	if err := json.Unmarshal(hb, &hdr); err != nil {
		return nil, unauth("header: %v", err)
	}
	if len(hdr.Crit) > 0 {
		return nil, unauth("unsupported crit header")
	}
	hash, ok := algHash(hdr.Alg)
	if !ok {
		// Covers `none`, every HS* (symmetric: the client secret would mint
		// tokens) and anything unknown.
		return nil, unauth("algorithm %q not allowed", hdr.Alg)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, unauth("signature encoding: %v", err)
	}
	key, err := v.keyFor(ctx, hdr.Kid)
	if err != nil {
		return nil, err
	}
	// The key decides the algorithm, not the token: a JWK that names its alg
	// must match, and the key type must fit the alg family (RFC 8725 §3.1).
	if key.alg != "" && key.alg != hdr.Alg {
		return nil, unauth("token alg %s does not match key %q alg %s", hdr.Alg, key.kid, key.alg)
	}
	h := hash.New()
	h.Write([]byte(parts[0] + "." + parts[1]))
	digest := h.Sum(nil)
	if err := verifyWith(key.pub, hdr.Alg, hash, digest, sig); err != nil {
		return nil, unauth("signature: %v", err)
	}
	pb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, unauth("payload encoding: %v", err)
	}
	var claims map[string]json.RawMessage
	if err := json.Unmarshal(pb, &claims); err != nil {
		return nil, unauth("payload: %v", err)
	}
	return claims, nil
}

func algHash(alg string) (crypto.Hash, bool) {
	switch alg {
	case "RS256", "PS256", "ES256":
		return crypto.SHA256, true
	case "RS384", "PS384", "ES384":
		return crypto.SHA384, true
	case "RS512", "PS512", "ES512":
		return crypto.SHA512, true
	}
	return 0, false
}

func verifyWith(pub crypto.PublicKey, alg string, hash crypto.Hash, digest, sig []byte) error {
	switch alg[:2] {
	case "RS", "PS":
		k, ok := pub.(*rsa.PublicKey)
		if !ok {
			return fmt.Errorf("alg %s needs an RSA key", alg)
		}
		if alg[0] == 'R' {
			return rsa.VerifyPKCS1v15(k, hash, digest, sig)
		}
		return rsa.VerifyPSS(k, hash, digest, sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: hash})
	case "ES":
		k, ok := pub.(*ecdsa.PublicKey)
		if !ok {
			return fmt.Errorf("alg %s needs an EC key", alg)
		}
		want := map[string]elliptic.Curve{"ES256": elliptic.P256(), "ES384": elliptic.P384(), "ES512": elliptic.P521()}[alg]
		if k.Curve != want {
			return fmt.Errorf("alg %s does not fit curve %s", alg, k.Curve.Params().Name)
		}
		// JWS carries R||S fixed-width (RFC 7518 §3.4), not ASN.1 DER.
		n := (k.Curve.Params().BitSize + 7) / 8
		if len(sig) != 2*n {
			return fmt.Errorf("ecdsa signature length %d, want %d", len(sig), 2*n)
		}
		r, s := new(big.Int).SetBytes(sig[:n]), new(big.Int).SetBytes(sig[n:])
		if !ecdsa.Verify(k, digest, r, s) {
			return errors.New("ecdsa verification failed")
		}
		return nil
	}
	return fmt.Errorf("alg %s not allowed", alg)
}

// ── key set ─────────────────────────────────────────────────────────────────

// keyFor returns the signing key for kid, refreshing the cached set when it is
// older than JWKSRefresh, and refetching once on an unknown kid (key rotation).
// Every network attempt is rate-limited to one per MinRefetch, so a stream of
// tokens with made-up kids cannot turn the kernel into a JWKS flooder.
func (v *OIDCVerifier) keyFor(ctx context.Context, kid string) (jwk, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.cfg.Now()
	mayFetch := v.attemptedAt.IsZero() || now.Sub(v.attemptedAt) >= v.cfg.MinRefetch

	if v.keys == nil || now.Sub(v.fetchedAt) > v.cfg.JWKSRefresh {
		if !mayFetch {
			if v.keys == nil {
				return jwk{}, unauth("issuer keys unavailable (next fetch attempt rate-limited)")
			}
		} else if err := v.refreshLocked(ctx, now); err != nil {
			if v.keys == nil {
				return jwk{}, err
			}
			// Tradeoff: an IdP outage keeps the last good key set in use rather
			// than logging every operator out; a rotated-out key therefore stays
			// accepted until the IdP is reachable again. exp still bounds each token.
			slog.WarnContext(ctx, "oidc: key set refresh failed; using the cached set", "issuer", v.cfg.Issuer, "err", err)
		}
		mayFetch = false
	}
	if k, ok := v.lookupLocked(kid); ok {
		return k, nil
	}
	if mayFetch && kid != "" {
		if err := v.refreshLocked(ctx, now); err != nil {
			return jwk{}, err
		}
		if k, ok := v.lookupLocked(kid); ok {
			return k, nil
		}
	}
	return jwk{}, unauth("no signing key %q in the issuer's key set", kid)
}

// lookupLocked finds kid. A token without a kid is accepted only when the set
// holds exactly one key — guessing among several would be trying keys until one fits.
func (v *OIDCVerifier) lookupLocked(kid string) (jwk, bool) {
	if kid == "" {
		if len(v.keys) == 1 {
			for _, k := range v.keys {
				return k, true
			}
		}
		return jwk{}, false
	}
	k, ok := v.keys[kid]
	return k, ok
}

// refreshLocked runs discovery (once, or again after a failure) and fetches the JWKS.
func (v *OIDCVerifier) refreshLocked(ctx context.Context, now time.Time) error {
	v.attemptedAt = now
	if v.jwksURI == "" {
		uri, err := v.discover(ctx)
		if err != nil {
			slog.WarnContext(ctx, "oidc: discovery failed; OIDC tokens are refused until it succeeds", "issuer", v.cfg.Issuer, "err", err)
			return unauth("discovery: %v", err)
		}
		v.jwksURI = uri
	}
	keys, err := v.fetchJWKS(ctx, v.jwksURI)
	if err != nil {
		v.jwksURI = "" // the jwks_uri may have moved: re-discover next time
		slog.WarnContext(ctx, "oidc: key set fetch failed", "issuer", v.cfg.Issuer, "err", err)
		return unauth("jwks: %v", err)
	}
	v.keys, v.fetchedAt = keys, now
	return nil
}

func (v *OIDCVerifier) discover(ctx context.Context) (string, error) {
	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	u := strings.TrimSuffix(v.cfg.Issuer, "/") + "/.well-known/openid-configuration"
	if err := v.getJSON(ctx, u, &doc); err != nil {
		return "", err
	}
	// OIDC Discovery §4.3: the document's issuer MUST equal the configured one,
	// otherwise a document served at this URL could vouch for another issuer.
	if doc.Issuer != v.cfg.Issuer {
		return "", fmt.Errorf("discovery issuer %q does not equal configured issuer %q", doc.Issuer, v.cfg.Issuer)
	}
	if doc.JWKSURI == "" {
		return "", errors.New("discovery document has no jwks_uri")
	}
	ju, err := url.Parse(doc.JWKSURI)
	if err != nil || !(ju.Scheme == "https" || ju.Scheme == "http" && isLoopbackHost(ju.Hostname())) {
		return "", fmt.Errorf("jwks_uri %q must be https", doc.JWKSURI)
	}
	return doc.JWKSURI, nil
}

func (v *OIDCVerifier) fetchJWKS(ctx context.Context, uri string) (map[string]jwk, error) {
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Use string `json:"use"`
			Alg string `json:"alg"`
			N   string `json:"n"`
			E   string `json:"e"`
			Crv string `json:"crv"`
			X   string `json:"x"`
			Y   string `json:"y"`
		} `json:"keys"`
	}
	if err := v.getJSON(ctx, uri, &set); err != nil {
		return nil, err
	}
	out := map[string]jwk{}
	for i, k := range set.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue // encryption keys never verify a signature
		}
		pub, err := parseJWKKey(k.Kty, k.N, k.E, k.Crv, k.X, k.Y)
		if err != nil {
			// One malformed or unsupported key must not take the whole set down.
			slog.DebugContext(ctx, "oidc: skipping JWK", "issuer", v.cfg.Issuer, "kid", k.Kid, "index", i, "err", err)
			continue
		}
		out[k.Kid] = jwk{kid: k.Kid, alg: k.Alg, pub: pub}
	}
	if len(out) == 0 {
		return nil, errors.New("key set holds no usable signing key")
	}
	return out, nil
}

func parseJWKKey(kty, n, e, crv, x, y string) (crypto.PublicKey, error) {
	dec := base64.RawURLEncoding.DecodeString
	switch kty {
	case "RSA":
		nb, err1 := dec(n)
		eb, err2 := dec(e)
		if err1 != nil || err2 != nil || len(nb) == 0 || len(eb) == 0 || len(eb) > 4 {
			return nil, errors.New("bad RSA modulus or exponent")
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(new(big.Int).SetBytes(eb).Int64())}
		if pub.N.BitLen() < minRSABits {
			return nil, fmt.Errorf("RSA key of %d bits is below %d", pub.N.BitLen(), minRSABits)
		}
		return pub, nil
	case "EC":
		var curve elliptic.Curve
		var ecdhCurve ecdh.Curve
		switch crv {
		case "P-256":
			curve, ecdhCurve = elliptic.P256(), ecdh.P256()
		case "P-384":
			curve, ecdhCurve = elliptic.P384(), ecdh.P384()
		case "P-521":
			curve, ecdhCurve = elliptic.P521(), ecdh.P521()
		default:
			return nil, fmt.Errorf("unsupported curve %q", crv)
		}
		xb, err1 := dec(x)
		yb, err2 := dec(y)
		size := (curve.Params().BitSize + 7) / 8
		// RFC 7518 §6.2.1.2/3: x and y are the full fixed-width coordinates.
		if err1 != nil || err2 != nil || len(xb) != size || len(yb) != size {
			return nil, errors.New("bad EC coordinates")
		}
		// Validate the point by parsing it as an uncompressed SEC 1 point with
		// crypto/ecdh, which rejects off-curve points and the point at infinity
		// (an invalid-curve key must never reach ecdsa.Verify). Rejected:
		// elliptic.Curve.IsOnCurve — deprecated, and it is the low-level API
		// the standard library steers callers away from.
		point := append(append([]byte{4}, xb...), yb...)
		if _, err := ecdhCurve.NewPublicKey(point); err != nil {
			return nil, fmt.Errorf("invalid EC point: %w", err)
		}
		return &ecdsa.PublicKey{Curve: curve, X: new(big.Int).SetBytes(xb), Y: new(big.Int).SetBytes(yb)}, nil
	}
	return nil, fmt.Errorf("unsupported kty %q", kty)
}

func (v *OIDCVerifier) getJSON(ctx context.Context, u string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := v.cfg.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", u, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDocumentBytes+1))
	if err != nil {
		return err
	}
	if len(body) > maxDocumentBytes {
		return fmt.Errorf("GET %s: body exceeds %d bytes", u, maxDocumentBytes)
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("GET %s: %w", u, err)
	}
	return nil
}

// ── claims ──────────────────────────────────────────────────────────────────

type verifiedClaims struct {
	iss, sub, email, name string
	// oid / tid are Entra ID's tenant-stable object id and tenant id. Entra's `sub`
	// is pairwise per application, so a SCIM push (externalId = objectId) and a
	// sign-in name the same person by different values; linkByOID joins them.
	oid, tid string
	exp      time.Time
}

func (v *OIDCVerifier) checkClaims(c map[string]json.RawMessage) (verifiedClaims, error) {
	var out verifiedClaims
	str := func(k string) string {
		var s string
		_ = json.Unmarshal(c[k], &s) // absent or non-string reads as "": checked below where required
		return s
	}
	out.iss, out.sub = str("iss"), str("sub")
	if out.iss != v.cfg.Issuer {
		return out, unauth("iss %q is not the configured issuer", out.iss)
	}
	if out.sub == "" {
		return out, unauth("sub missing")
	}
	if !v.audienceOK(c["aud"]) {
		return out, unauth("aud does not name an accepted audience")
	}
	// An ID token is not a bearer credential. It is minted for the client (OIDC
	// Core §2), its aud is the client id so it passes the audience check above,
	// it travels more freely (id_token_hint in logout URLs) and it is not revoked
	// with the access token. Keycloak marks its token kinds in a `typ` claim
	// ("ID" for ID tokens, "Bearer" for access tokens), so a token carrying
	// typ "ID" is refused here. Decided by the owner 2026-11-14 (ENG-714):
	// refuse Keycloak ID tokens only. Rejected for now: requiring the RFC 9068
	// header typ "at+jwt" (Entra never sets it, Keycloak only when configured)
	// and refusing any token with a `nonce` (Keycloak puts nonce into access
	// tokens too). The Entra rule is decided separately. Comparison is
	// case-insensitive so "id" cannot slip past.
	if typ := str("typ"); strings.EqualFold(typ, "ID") {
		return out, unauth("token typ %q is an ID token, not an access token", typ)
	}
	now, skew := v.cfg.Now(), v.cfg.ClockSkew
	exp, ok, err := numericDate(c["exp"])
	if err != nil || !ok {
		return out, unauth("exp missing or malformed")
	}
	if !now.Before(exp.Add(skew)) {
		return out, unauth("token expired at %s", exp.UTC().Format(time.RFC3339))
	}
	out.exp = exp
	if nbf, ok, err := numericDate(c["nbf"]); err != nil {
		return out, unauth("nbf malformed")
	} else if ok && now.Add(skew).Before(nbf) {
		return out, unauth("token not valid before %s", nbf.UTC().Format(time.RFC3339))
	}
	if iat, ok, err := numericDate(c["iat"]); err != nil {
		return out, unauth("iat malformed")
	} else if ok && now.Add(skew).Before(iat) {
		return out, unauth("token issued in the future (%s)", iat.UTC().Format(time.RFC3339))
	}
	out.email = str("email")
	out.oid = str("oid")
	out.tid = str("tid")
	out.name = str("name")
	if out.name == "" {
		out.name = str("preferred_username")
	}
	return out, nil
}

// audienceOK accepts `aud` as a string or an array (RFC 7519 §4.1.3).
//
// `azp` is deliberately NOT a substitute for `aud`. Keycloak's default access
// token carries `aud: ["account", …]` and `azp: <client>`, and accepting
// "azp == ClientID" would make that work out of the box. Rejected because `azp`
// says who REQUESTED the token, not who it is FOR: a token our console client
// obtained for another resource server (aud = that server) could then be
// replayed to the kernel by that server — the token-forwarding attack the
// audience check exists to stop (RFC 8725 §3.9, RFC 9068 §4). The supported
// Keycloak setup is an "Audience" protocol mapper (or a client scope with
// "Included Client Audience") that puts the client id, or a dedicated
// Cambrian audience listed in Audiences, into `aud`.
func (v *OIDCVerifier) audienceOK(raw json.RawMessage) bool {
	got := stringOrList(raw)
	for _, a := range got {
		for _, want := range v.cfg.Audiences {
			if a == want {
				return true
			}
		}
	}
	return false
}

func stringOrList(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []string{s}
	}
	var list []any
	if json.Unmarshal(raw, &list) != nil {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, x := range list {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// numericDate parses a JWT NumericDate (seconds, possibly fractional).
func numericDate(raw json.RawMessage) (time.Time, bool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}, false, nil
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return time.Time{}, false, err
	}
	sec := int64(f)
	return time.Unix(sec, int64((f-float64(sec))*1e9)), true, nil
}

// ── registry mapping ────────────────────────────────────────────────────────

// person maps (iss, sub) to a registry person. The email claim is stored on a
// newly created person for display but NEVER used to find one: an email is
// reassigned when someone leaves, and the next holder must not inherit the
// previous person's memory and approvals (ADR-0142 threat table).
func (v *OIDCVerifier) person(ctx context.Context, c verifiedClaims) (domain.Principal, error) {
	p, err := v.reg.ResolveIdentity(ctx, c.iss, c.sub)
	if errors.Is(err, domain.ErrIdentityNotFound) {
		var linked bool
		p, linked, err = v.linkByOID(ctx, c)
		if err == nil && !linked {
			p, err = v.firstLogin(ctx, c)
		}
	}
	if err != nil {
		return domain.Principal{}, err
	}
	if !p.Active() {
		return domain.Principal{}, unauth("person %s is %s", p.ID, p.Status)
	}
	return p, nil
}

// linkByOID joins an Entra sign-in to the person a SCIM directory provisioned
// (owner decision 2026-11-14, third round;
// T-20261106-entra-sign-in-and-entra-scim-provisioning). Entra's `sub` is pairwise
// per application while Entra SCIM sends externalId = the user's object id, and
// premium orgdir links a provisioned person as (connection OIDC issuer,
// externalId). So an unknown (iss, sub) whose `oid` claim is linked, under the
// SAME issuer, to a SCIM-provisioned person whose ExternalID is that oid is
// linked to that person: (iss, sub) → person, recorded once and audited, then
// resolved like any linked identity. Deterministic and idempotent: the same token
// always reaches the same person, and LinkIdentity never re-points a pair.
//
// Fail closed — any doubt is "no auto-link" (linked=false, err=nil), which hands
// the caller to the configured first-login policy and logs why:
//   - no oid, or oid == sub (nothing to join);
//   - no tid, a tid that is not a GUID, or an issuer whose path does not carry
//     that tid as a segment. Entra issuers are https://login.microsoftonline.com/
//     {tid}/v2.0 and https://sts.windows.net/{tid}/; this keeps the join to Entra
//     and to the configured tenant, so another IdP that happens to emit an `oid`
//     (a Keycloak user attribute mapped by an admin) cannot claim a person;
//   - the (iss, oid) link names nobody, or a person that is not SCIM-sourced or
//     whose ExternalID is not the oid;
//   - more than one SCIM-sourced person carries that ExternalID (ambiguous).
func (v *OIDCVerifier) linkByOID(ctx context.Context, c verifiedClaims) (domain.Principal, bool, error) {
	if c.oid == "" || c.oid == c.sub {
		return domain.Principal{}, false, nil
	}
	skip := func(reason string) (domain.Principal, bool, error) {
		sh := sha256.Sum256([]byte(c.oid))
		slog.WarnContext(ctx, "oidc: oid present but not linked to a provisioned person; no auto-link",
			"issuer", c.iss, "oid_sha256", hex.EncodeToString(sh[:8]), "reason", reason,
			"ticket", "T-20261106-entra-sign-in-and-entra-scim-provisioning")
		return domain.Principal{}, false, nil
	}
	if !isGUID(c.tid) || !issuerCarriesTenant(c.iss, c.tid) {
		return skip("tid missing, not a GUID, or not the configured issuer's tenant")
	}
	p, err := v.reg.ResolveIdentity(ctx, c.iss, c.oid)
	if errors.Is(err, domain.ErrIdentityNotFound) || errors.Is(err, domain.ErrPrincipalNotFound) {
		return skip("no person provisioned under this oid")
	}
	if err != nil {
		return domain.Principal{}, false, unauth("oid link: resolve: %v", err)
	}
	if p.Source != domain.SourceSCIM || p.ExternalID != c.oid {
		return skip("the oid names a person that is not a SCIM-provisioned person with that externalId")
	}
	scim, err := v.reg.ListPrincipals(ctx, domain.PrincipalFilter{Source: domain.SourceSCIM})
	if err != nil {
		return domain.Principal{}, false, unauth("oid link: list provisioned persons: %v", err)
	}
	n := 0
	for _, q := range scim {
		if q.ExternalID == c.oid {
			n++
		}
	}
	if n != 1 {
		return skip(fmt.Sprintf("ambiguous: %d provisioned persons carry this externalId", n))
	}
	if err := v.reg.LinkIdentity(ctx, domain.ExternalIdentity{
		Issuer: c.iss, Subject: c.sub, Email: c.email, PrincipalID: p.ID,
	}); err != nil {
		return domain.Principal{}, false, unauth("oid link: link identity: %v", err)
	}
	p, err = v.reg.ResolveIdentity(ctx, c.iss, c.sub)
	if err != nil {
		return domain.Principal{}, false, unauth("oid link: re-resolve: %v", err)
	}
	ssh := sha256.Sum256([]byte(c.sub))
	// The audit record of the join: who (principal), by which identity (issuer and
	// a subject hash, never the raw subject), and why (oid matched a SCIM person).
	slog.InfoContext(ctx, "oidc: first login linked to a SCIM-provisioned person by oid",
		"audit", "identity_linked", "issuer", c.iss, "principal", p.ID,
		"subject_sha256", hex.EncodeToString(ssh[:8]), "rule", "oid=scim_external_id")
	return p, true, nil
}

// isGUID reports whether s is a canonical 8-4-4-4-12 hex GUID (an Entra tenant id).
func isGUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
				return false
			}
		}
	}
	return true
}

// issuerCarriesTenant reports whether tid is a whole path segment of the issuer URL.
func issuerCarriesTenant(issuer, tid string) bool {
	u, err := url.Parse(issuer)
	if err != nil {
		return false
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if strings.EqualFold(seg, tid) {
			return true
		}
	}
	return false
}

func (v *OIDCVerifier) firstLogin(ctx context.Context, c verifiedClaims) (domain.Principal, error) {
	if v.cfg.FirstLogin != FirstLoginAutoCreate {
		sh := sha256.Sum256([]byte(c.sub))
		slog.WarnContext(ctx, "oidc: verified identity is not linked to a registry person; refusing (first_login=require_provisioned). Fix: link the identity to a registry person first",
			"issuer", c.iss, "subject_sha256", hex.EncodeToString(sh[:8]))
		return domain.Principal{}, unauth("identity not provisioned")
	}
	id := domain.PersonIDFor("oidc", c.iss, c.sub)
	if _, _, err := v.reg.EnsurePrincipal(ctx, domain.Principal{
		ID:          id,
		Kind:        domain.PrincipalUser,
		DisplayName: c.name,
		Email:       c.email,
		Source:      domain.SourceOIDC,
		ExternalID:  c.sub,
		Roles:       append([]string(nil), v.cfg.DefaultRoles...),
	}); err != nil {
		return domain.Principal{}, unauth("first login: create person: %v", err)
	}
	// Linking is idempotent for the same pair → same person, so two concurrent
	// first logins of one subject both succeed.
	if err := v.reg.LinkIdentity(ctx, domain.ExternalIdentity{
		Issuer: c.iss, Subject: c.sub, Email: c.email, PrincipalID: id,
	}); err != nil {
		return domain.Principal{}, unauth("first login: link identity: %v", err)
	}
	p, err := v.reg.ResolveIdentity(ctx, c.iss, c.sub)
	if err != nil {
		return domain.Principal{}, unauth("first login: re-resolve: %v", err)
	}
	slog.InfoContext(ctx, "oidc: first login created a registry person", "issuer", c.iss, "principal", id)
	return p, nil
}

// roles is the sorted union of the person's stored roles and the statically
// mapped values of the roles claim. The IdP's role names are never used raw:
// an unmapped group grants nothing (fail closed).
func (v *OIDCVerifier) roles(stored []string, claim json.RawMessage) []string {
	set := map[string]bool{}
	for _, r := range stored {
		set[r] = true
	}
	for _, val := range stringOrList(claim) {
		for _, r := range v.cfg.RoleMap[val] {
			set[r] = true
		}
	}
	out := make([]string, 0, len(set))
	for r := range set {
		if r != "" {
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return out
}

// Compile-time port checks.
var (
	_ domain.Authenticator      = (*OIDCVerifier)(nil)
	_ domain.AuthMetadataSource = (*OIDCVerifier)(nil)
)
