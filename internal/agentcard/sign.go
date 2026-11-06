package agentcard

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
)

// Signing (A2A §8.4.2) and verification (§8.4.3).
//
// The signing input is the JWS one (RFC 7515 §5.1):
//
//	BASE64URL(protected) "." BASE64URL(JCS(card without "signatures", defaults removed))
//
// and the card carries only `protected` and `signature` — the payload is the
// card itself, so the JWS is detached (RFC 7515 Appendix F). The kernel signs
// with EdDSA/Ed25519, for the reason premium receipts do (receipts/signer.go):
// deterministic signatures, small keys, constant time in the standard library,
// no curve or padding parameters to weaken. Verification also accepts ES256,
// the algorithm the A2A specification's own example uses, so cards from other
// A2A implementations can be checked.
//
// Threat model, stated plainly: the key lives with the deployment (its
// encrypted config store or a 0600 key file), so whoever operates the kernel
// can sign any card. A signature proves "this kernel published this card
// unchanged", which is what an A2A client needs to trust a card fetched over
// a path it does not control; it does not prove the agent behaves as declared.

// Header names and values.
const (
	AlgEdDSA = "EdDSA"
	AlgES256 = "ES256"
	typJOSE  = "JOSE"
)

// Errors a verification returns. Callers branch on them (RemoteFetcher's policy).
var (
	// ErrUnsigned: the card carries no signatures.
	ErrUnsigned = errors.New("agentcard: card is not signed")
	// ErrUnknownKey: no signature names a key in the trusted set.
	ErrUnknownKey = errors.New("agentcard: no signature is by a trusted key")
	// ErrBadSignature: a signature by a trusted key does not verify — the card
	// was altered after signing, or the signature is forged.
	ErrBadSignature = errors.New("agentcard: signature does not verify")
)

// Signer signs cards with one Ed25519 key. The private key is never exposed:
// no accessor returns it, and it is never formatted into a log or error.
type Signer struct {
	priv ed25519.PrivateKey
	kid  string
	jku  string
}

// NewSigner builds a signer from a 32-byte seed or a 64-byte private key
// (operators supply both; mis-deriving one from the other yields a signer that
// verifies against nothing, the receipts lesson). jku, optional, is the URL of
// the JWKS a verifier can fetch the public key from.
func NewSigner(key []byte, jku string) (*Signer, error) {
	var priv ed25519.PrivateKey
	switch len(key) {
	case ed25519.SeedSize:
		priv = ed25519.NewKeyFromSeed(key)
	case ed25519.PrivateKeySize:
		priv = append(ed25519.PrivateKey(nil), key...)
		// A 64-byte key whose public half does not match its seed is corrupt.
		if !ed25519.PublicKey(priv[32:]).Equal(ed25519.NewKeyFromSeed(priv[:32]).Public()) {
			return nil, errors.New("agentcard: signing key is corrupt (public half does not match the seed)")
		}
	default:
		return nil, fmt.Errorf("agentcard: signing key must be %d or %d bytes, got %d",
			ed25519.SeedSize, ed25519.PrivateKeySize, len(key))
	}
	pub := priv.Public().(ed25519.PublicKey)
	return &Signer{priv: priv, kid: Thumbprint(pub), jku: jku}, nil
}

// ParseKey decodes a stored key: hex (the receipts convention) or base64url /
// base64 of a 32-byte seed or 64-byte private key.
func ParseKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if b, err := hex.DecodeString(s); err == nil && (len(b) == ed25519.SeedSize || len(b) == ed25519.PrivateKeySize) {
		return b, nil
	}
	for _, enc := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.StdEncoding, base64.RawStdEncoding} {
		if b, err := enc.DecodeString(s); err == nil && (len(b) == ed25519.SeedSize || len(b) == ed25519.PrivateKeySize) {
			return b, nil
		}
	}
	// The value is deliberately not echoed: it is key material.
	return nil, errors.New("agentcard: stored signing key is not hex or base64 of a 32-byte seed or 64-byte key")
}

// GenerateKey mints a new Ed25519 seed, hex-encoded for storage.
func GenerateKey() (string, error) {
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return "", fmt.Errorf("agentcard: generate signing key: %w", err)
	}
	return hex.EncodeToString(priv.Seed()), nil
}

// LoadOrCreateKeyFile reads the hex seed at path, creating it (0600, parent
// 0700) with a fresh key when absent. created reports a first-boot mint.
func LoadOrCreateKeyFile(path string) (key []byte, created bool, err error) {
	b, err := os.ReadFile(path)
	if err == nil {
		k, perr := ParseKey(string(b))
		if perr != nil {
			return nil, false, fmt.Errorf("%w (file %s)", perr, path)
		}
		return k, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, fmt.Errorf("agentcard: read signing key %s: %w", path, err)
	}
	h, err := GenerateKey()
	if err != nil {
		return nil, false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, fmt.Errorf("agentcard: create key directory: %w", err)
	}
	// O_EXCL: two kernels racing a first boot must not each mint and overwrite.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return LoadOrCreateKeyFile(path)
		}
		return nil, false, fmt.Errorf("agentcard: create signing key %s: %w", path, err)
	}
	if _, err := f.WriteString(h + "\n"); err != nil {
		_ = f.Close()
		return nil, false, fmt.Errorf("agentcard: write signing key: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, false, fmt.Errorf("agentcard: write signing key: %w", err)
	}
	k, _ := hex.DecodeString(h)
	return k, true, nil
}

// KeyID is the signer's kid: the RFC 7638 JWK thumbprint of its public key.
func (s *Signer) KeyID() string { return s.kid }

// PublicKey returns the verification key.
func (s *Signer) PublicKey() ed25519.PublicKey { return s.priv.Public().(ed25519.PublicKey) }

// JWKS is the signer's public key as a JSON Web Key Set (RFC 7517), served at
// /.well-known/jwks.json so a verifier can pin it.
func (s *Signer) JWKS() JWKS {
	return JWKS{Keys: []JWK{{
		Kty: "OKP", Crv: "Ed25519", X: base64.RawURLEncoding.EncodeToString(s.PublicKey()),
		Kid: s.kid, Use: "sig", Alg: AlgEdDSA,
	}}}
}

// Sign returns c with one kernel signature replacing any it carried.
func (s *Signer) Sign(c Card) (Card, error) {
	c.Signatures = nil
	payload, err := SigningPayload(c)
	if err != nil {
		return Card{}, err
	}
	hdr := map[string]string{"alg": AlgEdDSA, "kid": s.kid, "typ": typJOSE}
	if s.jku != "" {
		hdr["jku"] = s.jku
	}
	hb, err := CanonicalizeValue(hdr)
	if err != nil {
		return Card{}, err
	}
	protected := base64.RawURLEncoding.EncodeToString(hb)
	input := protected + "." + base64.RawURLEncoding.EncodeToString(payload)
	sig := ed25519.Sign(s.priv, []byte(input))
	c.Signatures = []Signature{{Protected: protected, Signature: base64.RawURLEncoding.EncodeToString(sig)}}
	return c, nil
}

// SigningPayload is the canonical bytes a card's signature covers.
func SigningPayload(c Card) ([]byte, error) {
	c.Signatures = nil
	b, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("agentcard: marshal card: %w", err)
	}
	return payloadFromRaw(b)
}

// payloadFromRaw applies §8.4.1 to a card's JSON: drop `signatures`, drop the
// default-valued optional members, canonicalise. Duplicate member names are
// refused by Canonicalize.
func payloadFromRaw(raw []byte) ([]byte, error) {
	// Canonicalise first so a duplicate key is refused before anything reads it.
	canon, err := Canonicalize(raw)
	if err != nil {
		return nil, err
	}
	var v map[string]any
	if err := json.Unmarshal(canon, &v); err != nil {
		return nil, fmt.Errorf("agentcard: card is not a JSON object: %w", err)
	}
	delete(v, "signatures")
	stripDefaults(v, cardRequired)
	if caps, ok := v["capabilities"].(map[string]any); ok {
		stripDefaults(caps, nil)
		if exts, ok := caps["extensions"].([]any); ok {
			for _, e := range exts {
				if m, ok := e.(map[string]any); ok {
					stripDefaults(m, nil)
				}
			}
		}
	}
	if p, ok := v["provider"].(map[string]any); ok {
		stripDefaults(p, providerRequired)
	}
	for _, list := range []struct {
		key string
		req map[string]bool
	}{{"skills", skillRequired}, {"supportedInterfaces", interfaceRequired}} {
		if items, ok := v[list.key].([]any); ok {
			for _, it := range items {
				if m, ok := it.(map[string]any); ok {
					stripDefaults(m, list.req)
				}
			}
		}
	}
	return CanonicalizeValue(v)
}

// Required members per §4.4; every other member at these levels is removed
// when it holds its default (false, 0, "", [], {} or null).
var (
	cardRequired = set("name", "description", "supportedInterfaces", "version", "capabilities",
		"defaultInputModes", "defaultOutputModes", "skills")
	providerRequired  = set("organization", "url")
	skillRequired     = set("id", "name", "description", "tags")
	interfaceRequired = set("url", "protocolBinding", "protocolVersion")
)

func set(keys ...string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

func stripDefaults(m map[string]any, required map[string]bool) {
	for k, v := range m {
		if required[k] {
			continue
		}
		if isDefault(v) {
			delete(m, k)
		}
	}
}

func isDefault(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case bool:
		return !x
	case float64:
		return x == 0
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

// ── keys and verification ────────────────────────────────────────────────────

// JWK is the subset of RFC 7517 the kernel reads and writes: OKP/Ed25519 and EC/P-256.
type JWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv,omitempty"`
	X   string `json:"x,omitempty"`
	Y   string `json:"y,omitempty"`
	Kid string `json:"kid,omitempty"`
	Use string `json:"use,omitempty"`
	Alg string `json:"alg,omitempty"`
}

// JWKS is a JSON Web Key Set.
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// KeySet maps a kid to a trusted verification key (ed25519.PublicKey or *ecdsa.PublicKey).
type KeySet map[string]crypto.PublicKey

// Thumbprint is the RFC 7638 JWK thumbprint (SHA-256, base64url) of an Ed25519 key.
func Thumbprint(pub ed25519.PublicKey) string {
	// Members in lexicographic order, no whitespace: RFC 7638 §3.2.
	in := `{"crv":"Ed25519","kty":"OKP","x":"` + base64.RawURLEncoding.EncodeToString(pub) + `"}`
	sum := sha256.Sum256([]byte(in))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ParseJWKS reads a key set. A key without a kid is keyed by its thumbprint
// when it is Ed25519; an unsupported key type is an error, not a skip, so a
// trust file that silently trusts nothing cannot be written.
func ParseJWKS(b []byte) (KeySet, error) {
	var set JWKS
	if err := json.Unmarshal(b, &set); err != nil {
		return nil, fmt.Errorf("agentcard: parse JWKS: %w", err)
	}
	ks := KeySet{}
	for i, k := range set.Keys {
		switch {
		case k.Kty == "OKP" && k.Crv == "Ed25519":
			x, err := base64.RawURLEncoding.DecodeString(k.X)
			if err != nil || len(x) != ed25519.PublicKeySize {
				return nil, fmt.Errorf("agentcard: JWKS key %d: bad Ed25519 x", i)
			}
			kid := k.Kid
			if kid == "" {
				kid = Thumbprint(x)
			}
			ks[kid] = ed25519.PublicKey(x)
		case k.Kty == "EC" && k.Crv == "P-256":
			x, errX := base64.RawURLEncoding.DecodeString(k.X)
			y, errY := base64.RawURLEncoding.DecodeString(k.Y)
			if errX != nil || errY != nil || len(x) != 32 || len(y) != 32 {
				return nil, fmt.Errorf("agentcard: JWKS key %d: bad P-256 coordinates", i)
			}
			pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
			if !pub.Curve.IsOnCurve(pub.X, pub.Y) { //nolint:staticcheck // validating untrusted input
				return nil, fmt.Errorf("agentcard: JWKS key %d: point not on P-256", i)
			}
			if k.Kid == "" {
				return nil, fmt.Errorf("agentcard: JWKS key %d: an EC key needs a kid", i)
			}
			ks[k.Kid] = pub
		default:
			return nil, fmt.Errorf("agentcard: JWKS key %d: unsupported key type %s/%s (EdDSA Ed25519 and ES256 P-256 only)", i, k.Kty, k.Crv)
		}
	}
	return ks, nil
}

// Verification is the outcome of a successful Verify.
type Verification struct {
	KeyID     string
	Algorithm string
}

// Verify checks a card's JSON against the trusted keys. It succeeds when at
// least one signature by a trusted key verifies (§8.4.3: clients SHOULD verify
// at least one). The errors are ordered by severity: any signature by a
// trusted key that FAILS makes the card ErrBadSignature even if another
// verifies, because a card carrying a broken signature by a key we trust has
// been tampered with or assembled from parts.
func Verify(raw []byte, keys KeySet) (Verification, error) {
	var env struct {
		Signatures []Signature `json:"signatures"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return Verification{}, fmt.Errorf("agentcard: card is not valid JSON: %w", err)
	}
	if len(env.Signatures) == 0 {
		return Verification{}, ErrUnsigned
	}
	payload, err := payloadFromRaw(raw)
	if err != nil {
		return Verification{}, err
	}
	b64payload := base64.RawURLEncoding.EncodeToString(payload)
	var ok *Verification
	for _, sig := range env.Signatures {
		hb, err := base64.RawURLEncoding.DecodeString(sig.Protected)
		if err != nil {
			return Verification{}, fmt.Errorf("%w: protected header is not base64url", ErrBadSignature)
		}
		var hdr map[string]any
		if err := json.Unmarshal(hb, &hdr); err != nil {
			return Verification{}, fmt.Errorf("%w: protected header is not JSON", ErrBadSignature)
		}
		if _, crit := hdr["crit"]; crit {
			// RFC 7515 §4.1.11: an extension we do not understand MUST fail.
			return Verification{}, fmt.Errorf("%w: unsupported crit header", ErrBadSignature)
		}
		alg, _ := hdr["alg"].(string)
		kid, _ := hdr["kid"].(string)
		key, trusted := keys[kid]
		if kid == "" || !trusted {
			continue
		}
		sb, err := base64.RawURLEncoding.DecodeString(sig.Signature)
		if err != nil {
			return Verification{}, fmt.Errorf("%w: signature is not base64url (kid %s)", ErrBadSignature, kid)
		}
		input := []byte(sig.Protected + "." + b64payload)
		if !verifyOne(alg, key, input, sb) {
			return Verification{}, fmt.Errorf("%w (kid %s, alg %s)", ErrBadSignature, kid, alg)
		}
		if ok == nil {
			ok = &Verification{KeyID: kid, Algorithm: alg}
		}
	}
	if ok == nil {
		return Verification{}, ErrUnknownKey
	}
	return *ok, nil
}

// verifyOne checks one signature; the algorithm must match the key's type
// (an "alg" that does not fit the trusted key is a failure, never a downgrade).
func verifyOne(alg string, key crypto.PublicKey, input, sig []byte) bool {
	switch k := key.(type) {
	case ed25519.PublicKey:
		return alg == AlgEdDSA && ed25519.Verify(k, input, sig)
	case *ecdsa.PublicKey:
		if alg != AlgES256 || len(sig) != 64 {
			return false
		}
		sum := sha256.Sum256(input)
		r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
		return ecdsa.Verify(k, sum[:], r, s)
	}
	return false
}
