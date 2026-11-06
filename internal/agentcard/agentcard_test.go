package agentcard

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/agentprofile"
)

var updateGolden = flag.Bool("update-agentcard-golden", false, "rewrite testdata/*.golden.json")

// A fixed profile, so the golden card changes only when the card format does.
const goldenProfile = `name: researcher
version: 1.2.0
description: Researches questions on the web and answers with citations.
role: "You are a meticulous researcher."
capabilities:
  - web_research
  - research
skills:
  - web-research
tools:
  - mcp:web-search/*
  - mcp:fetch/*
examples:
  - Find three independent sources on a claim and summarise them
  - Check whether a quoted statistic is supported by primary sources
  - Compare two products using their published documentation
trust_tier: first-party
`

var testSeed = bytes.Repeat([]byte{7}, ed25519.SeedSize)

// profileAgent builds the registered definition + manifest the way discovery
// does (internal/storage/profile_discovery.go → mapper), from profile text.
func profileAgent(t *testing.T, text string) (domain.AgentDefinition, *domain.AgentManifest) {
	t.Helper()
	p, err := agentprofile.Parse(text, agentprofile.Options{ExpectedName: "researcher"})
	if err != nil {
		t.Fatal(err)
	}
	def := domain.AgentDefinition{
		ID: p.Name, Name: "Researcher", Description: p.Description, Runtime: domain.RuntimePython,
		SourceHash: "sha256:0123456789abcdef0123456789abcdef", ManifestVersion: p.Version,
		Profile: p.Name, TrustTier: p.TrustTier,
	}
	man := &domain.AgentManifest{
		Version: p.Version, Capabilities: p.Capabilities, SupportedFormats: []string{"text"},
		ToolAllowlist: p.Tools, Skills: p.Skills, Examples: p.Examples,
	}
	return def, man
}

func testOptions() BuildOptions {
	return BuildOptions{
		Provider:   Provider{Organization: "Example Org", URL: "https://example.org"},
		Interfaces: []InterfaceTemplate{{URLTemplate: "https://agents.example.org/a2a/{agent_id}", ProtocolBinding: "JSONRPC"}},
	}
}

func testSigner(t *testing.T) *Signer {
	t.Helper()
	s, err := NewSigner(testSeed, "https://cards.example.org/.well-known/jwks.json")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (regenerate with -update-agentcard-golden): %v", err)
	}
	if !bytes.Equal(bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")), got) {
		t.Fatalf("%s is stale; regenerate with -update-agentcard-golden\ngot:\n%s", path, got)
	}
}

func TestBuildCardFromProfileGolden(t *testing.T) {
	def, man := profileAgent(t, goldenProfile)
	signed, err := testSigner(t).Sign(Build(def, man, testOptions()))
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.MarshalIndent(signed, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "researcher.golden.json", append(got, '\n'))

	// The properties the golden file encodes, asserted directly so a careless
	// regeneration cannot flip them.
	if signed.Version != "1.2.0+0123456789ab" {
		t.Errorf("version = %q", signed.Version)
	}
	if len(signed.Skills) != 2 || signed.Skills[0].ID != "research" || len(signed.Skills[0].Examples) != 3 {
		t.Errorf("skills = %+v", signed.Skills)
	}
	params := signed.Capabilities.Extensions[0].Params
	if params["trust_tier"] != TierFirstParty || len(params["tool_allowlist"].([]string)) != 2 {
		t.Errorf("extension params = %+v", params)
	}
	if signed.SupportedInterfaces[0].URL != "https://agents.example.org/a2a/researcher" ||
		signed.SupportedInterfaces[0].ProtocolVersion != ProtocolVersion {
		t.Errorf("interfaces = %+v", signed.SupportedInterfaces)
	}
	if signed.SecuritySchemes[bearerSchemeKey].HTTPAuth.Scheme != "Bearer" {
		t.Error("bearer scheme missing")
	}
	if signed.Capabilities.Streaming || signed.Capabilities.PushNotifications {
		t.Error("the kernel serves no A2A task endpoint; streaming/push must be false")
	}
}

func TestBuildWithoutDeploymentFacts(t *testing.T) {
	def, man := profileAgent(t, goldenProfile)
	c := Build(def, man, BuildOptions{})
	b, _ := json.Marshal(c)
	s := string(b)
	// Required members stay even when empty; unset optional ones are absent.
	if !strings.Contains(s, `"supportedInterfaces":[]`) || strings.Contains(s, `"provider"`) ||
		strings.Contains(s, `"documentationUrl"`) || strings.Contains(s, `"streaming"`) {
		t.Fatalf("card JSON = %s", s)
	}
	if c.DefaultInputModes[0] != "text/plain" {
		t.Errorf("modes = %v", c.DefaultInputModes)
	}
}

func TestEligibleAndTrustTier(t *testing.T) {
	cases := []struct {
		def      domain.AgentDefinition
		incl     bool
		eligible bool
		tier     string
	}{
		{domain.AgentDefinition{ID: "p", Profile: "p", TrustTier: "first-party"}, false, true, TierFirstParty},
		{domain.AgentDefinition{ID: "p", Profile: "p", TrustTier: "third-party"}, false, true, TierThirdParty},
		// A profile cannot grant itself system: only the composition root can.
		{domain.AgentDefinition{ID: "p", Profile: "p", TrustTier: "system"}, false, true, TierFirstParty},
		{domain.AgentDefinition{ID: "local"}, false, true, TierFirstParty},
		{domain.AgentDefinition{ID: "remote", Runtime: domain.RuntimeA2A, TrustTier: "system"}, false, false, TierThirdParty},
		{domain.AgentDefinition{ID: "gen", Trait: domain.TraitModel}, true, false, TierFirstParty},
		{domain.AgentDefinition{ID: "organ", System: true}, false, false, TierSystem},
		{domain.AgentDefinition{ID: "organ", System: true}, true, true, TierSystem},
	}
	for i, c := range cases {
		if ok, _ := Eligible(c.def, c.incl); ok != c.eligible {
			t.Errorf("%d: Eligible = %v, want %v", i, ok, c.eligible)
		}
		if got := TrustTier(c.def); got != c.tier {
			t.Errorf("%d: TrustTier = %q, want %q", i, got, c.tier)
		}
	}
}

func signedCardJSON(t *testing.T) ([]byte, *Signer) {
	t.Helper()
	def, man := profileAgent(t, goldenProfile)
	s := testSigner(t)
	c, err := s.Sign(Build(def, man, testOptions()))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return b, s
}

func keysOf(s *Signer) KeySet { return KeySet{s.KeyID(): s.PublicKey()} }

func TestSignVerifyRoundTrip(t *testing.T) {
	raw, s := signedCardJSON(t)
	v, err := Verify(raw, keysOf(s))
	if err != nil {
		t.Fatal(err)
	}
	if v.KeyID != s.KeyID() || v.Algorithm != AlgEdDSA {
		t.Fatalf("verification = %+v", v)
	}
	// The protected header is what A2A §8.4.2 asks for.
	var c Card
	_ = json.Unmarshal(raw, &c)
	hb, _ := base64.RawURLEncoding.DecodeString(c.Signatures[0].Protected)
	if string(hb) != `{"alg":"EdDSA","jku":"https://cards.example.org/.well-known/jwks.json","kid":"`+s.KeyID()+`","typ":"JOSE"}` {
		t.Fatalf("protected header = %s", hb)
	}
	// Whitespace, member order and an explicit default (streaming:false) do not
	// change what is signed: verification is over the canonical, default-free form.
	var generic map[string]any
	_ = json.Unmarshal(raw, &generic)
	generic["capabilities"].(map[string]any)["streaming"] = false
	pretty, _ := json.MarshalIndent(generic, "", "    ")
	if _, err := Verify(pretty, keysOf(s)); err != nil {
		t.Fatalf("re-encoded card no longer verifies: %v", err)
	}
	// The JWKS the listener serves verifies the card too.
	jwks, _ := json.Marshal(s.JWKS())
	ks, err := ParseJWKS(jwks)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(raw, ks); err != nil {
		t.Fatalf("verify via JWKS: %v", err)
	}
}

func TestVerifyDetectsTampering(t *testing.T) {
	raw, s := signedCardJSON(t)
	for name, mutate := range map[string]func(m map[string]any){
		"description": func(m map[string]any) { m["description"] = "Exfiltrates your files." },
		"trust tier": func(m map[string]any) {
			ext := m["capabilities"].(map[string]any)["extensions"].([]any)[0].(map[string]any)
			ext["params"].(map[string]any)["trust_tier"] = "system"
		},
		"tool allowlist": func(m map[string]any) {
			ext := m["capabilities"].(map[string]any)["extensions"].([]any)[0].(map[string]any)
			ext["params"].(map[string]any)["tool_allowlist"] = []any{"mcp:shell/*"}
		},
		"interface url": func(m map[string]any) {
			m["supportedInterfaces"].([]any)[0].(map[string]any)["url"] = "https://evil.example/a2a"
		},
		"added skill": func(m map[string]any) {
			m["skills"] = append(m["skills"].([]any), map[string]any{"id": "x", "name": "X", "description": "x", "tags": []any{"x"}})
		},
		"streaming claimed": func(m map[string]any) { m["capabilities"].(map[string]any)["streaming"] = true },
	} {
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		mutate(m)
		b, _ := json.Marshal(m)
		if _, err := Verify(b, keysOf(s)); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: err = %v, want ErrBadSignature", name, err)
		}
	}
	// A swapped signature byte.
	var c Card
	_ = json.Unmarshal(raw, &c)
	sig, _ := base64.RawURLEncoding.DecodeString(c.Signatures[0].Signature)
	sig[0] ^= 1
	c.Signatures[0].Signature = base64.RawURLEncoding.EncodeToString(sig)
	b, _ := json.Marshal(c)
	if _, err := Verify(b, keysOf(s)); !errors.Is(err, ErrBadSignature) {
		t.Errorf("flipped signature: err = %v", err)
	}
	// alg confusion: an ES256 header over an Ed25519 key never verifies.
	_ = json.Unmarshal(raw, &c)
	hdr, _ := CanonicalizeValue(map[string]string{"alg": "ES256", "kid": s.KeyID()})
	c.Signatures[0].Protected = base64.RawURLEncoding.EncodeToString(hdr)
	b, _ = json.Marshal(c)
	if _, err := Verify(b, keysOf(s)); !errors.Is(err, ErrBadSignature) {
		t.Errorf("alg confusion: err = %v", err)
	}
	// A duplicate member name smuggled into a signed card is refused outright.
	dup := bytes.Replace(raw, []byte(`{"name":`), []byte(`{"name":"Evil","name":`), 1)
	if _, err := Verify(dup, keysOf(s)); !errors.Is(err, ErrDuplicateKey) {
		t.Errorf("duplicate key: err = %v", err)
	}
}

func TestVerifyUnsignedAndUnknownKey(t *testing.T) {
	raw, _ := signedCardJSON(t)
	other, _ := NewSigner(bytes.Repeat([]byte{9}, 32), "")
	if _, err := Verify(raw, keysOf(other)); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("unknown key: err = %v", err)
	}
	def, man := profileAgent(t, goldenProfile)
	b, _ := json.Marshal(Build(def, man, BuildOptions{}))
	if _, err := Verify(b, keysOf(other)); !errors.Is(err, ErrUnsigned) {
		t.Errorf("unsigned: err = %v", err)
	}
}

// A card signed with ES256 by another A2A implementation verifies.
func TestVerifyES256(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	card := []byte(`{"name":"Foreign","description":"d","supportedInterfaces":[],"version":"1","capabilities":{},` +
		`"defaultInputModes":["text/plain"],"defaultOutputModes":["text/plain"],"skills":[]}`)
	payload, err := payloadFromRaw(card)
	if err != nil {
		t.Fatal(err)
	}
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"ES256","kid":"k1","typ":"JOSE"}`))
	sum := sha256.Sum256([]byte(hdr + "." + base64.RawURLEncoding.EncodeToString(payload)))
	r, s, err := ecdsa.Sign(rand.Reader, priv, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	var m map[string]any
	_ = json.Unmarshal(card, &m)
	m["signatures"] = []any{map[string]any{"protected": hdr, "signature": base64.RawURLEncoding.EncodeToString(sig)}}
	signed, _ := json.Marshal(m)

	jwks := `{"keys":[{"kty":"EC","crv":"P-256","kid":"k1","x":"` + b64(priv.X) + `","y":"` + b64(priv.Y) + `"}]}`
	ks, err := ParseJWKS([]byte(jwks))
	if err != nil {
		t.Fatal(err)
	}
	if v, err := Verify(signed, ks); err != nil || v.Algorithm != AlgES256 {
		t.Fatalf("ES256: %+v, %v", v, err)
	}
}

func b64(n *big.Int) string {
	b := make([]byte, 32)
	n.FillBytes(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func TestParseJWKSRefusesUnsupportedKeys(t *testing.T) {
	for _, in := range []string{
		`{"keys":[{"kty":"RSA","n":"x","e":"AQAB","kid":"r"}]}`,
		`{"keys":[{"kty":"OKP","crv":"Ed25519","x":"short"}]}`,
		`{"keys":[{"kty":"EC","crv":"P-256","x":"` + strings.Repeat("A", 43) + `","y":"` + strings.Repeat("A", 43) + `","kid":"z"}]}`,
	} {
		if _, err := ParseJWKS([]byte(in)); err == nil {
			t.Errorf("ParseJWKS(%s): want an error", in)
		}
	}
}

func TestKeyHandling(t *testing.T) {
	// Seed and full private key give the same signer.
	a, _ := NewSigner(testSeed, "")
	b, err := NewSigner(ed25519.NewKeyFromSeed(testSeed), "")
	if err != nil || a.KeyID() != b.KeyID() {
		t.Fatalf("seed vs private key: %v %s %s", err, a.KeyID(), b.KeyID())
	}
	if _, err := NewSigner([]byte("short"), ""); err == nil {
		t.Error("short key accepted")
	}
	bad := ed25519.NewKeyFromSeed(testSeed)
	bad[40] ^= 1
	if _, err := NewSigner(bad, ""); err == nil {
		t.Error("corrupt 64-byte key accepted")
	}

	// LoadOrCreateKeyFile mints once, then reads back the same key.
	path := filepath.Join(t.TempDir(), "keys", "a2a.key")
	k1, created, err := LoadOrCreateKeyFile(path)
	if err != nil || !created {
		t.Fatalf("first boot: %v created=%v", err, created)
	}
	k2, created, err := LoadOrCreateKeyFile(path)
	if err != nil || created || !bytes.Equal(k1, k2) {
		t.Fatalf("second boot: %v created=%v same=%v", err, created, bytes.Equal(k1, k2))
	}
	if st, _ := os.Stat(path); st.Size() == 0 {
		t.Error("key file empty")
	}
	// Garbage in the file is an error that does not echo the content.
	_ = os.WriteFile(path, []byte("not-a-key-SECRETVALUE"), 0o600)
	if _, _, err := LoadOrCreateKeyFile(path); err == nil || strings.Contains(err.Error(), "SECRETVALUE") {
		t.Errorf("bad key file: err = %v", err)
	}
	// Hex, base64url and base64 all parse.
	h, _ := GenerateKey()
	raw, _ := ParseKey(h)
	for _, enc := range []string{base64.RawURLEncoding.EncodeToString(raw), base64.StdEncoding.EncodeToString(raw)} {
		if k, err := ParseKey(enc); err != nil || !bytes.Equal(k, raw) {
			t.Errorf("ParseKey(%s): %v", enc, err)
		}
	}
}

// ── remote policy ────────────────────────────────────────────────────────────

type fakeFetcher struct{ card *domain.AgentCard }

func (f fakeFetcher) FetchCard(context.Context, string) (*domain.AgentCard, error) {
	c := *f.card
	return &c, nil
}

func remoteCard(raw []byte) *domain.AgentCard {
	var c domain.AgentCard
	_ = json.Unmarshal(raw, &c)
	c.Raw = raw
	return &c
}

func TestRemoteFetcherPolicies(t *testing.T) {
	signed, s := signedCardJSON(t)
	trusted := keysOf(s)
	var m map[string]any
	_ = json.Unmarshal(signed, &m)
	m["description"] = "tampered"
	tampered, _ := json.Marshal(m)
	delete(m, "signatures")
	unsigned, _ := json.Marshal(m)

	cases := []struct {
		name   string
		policy string
		raw    []byte
		keys   KeySet
		ok     bool
	}{
		{"required+valid", PolicyRequired, signed, trusted, true},
		{"required+bad signature", PolicyRequired, tampered, trusted, false},
		{"required+unsigned", PolicyRequired, unsigned, trusted, false},
		{"required+untrusted key", PolicyRequired, signed, KeySet{}, false},
		{"optional+valid", PolicyOptional, signed, trusted, true},
		{"optional+bad signature", PolicyOptional, tampered, trusted, false},
		{"optional+unsigned", PolicyOptional, unsigned, trusted, true},
		{"optional+untrusted key", PolicyOptional, signed, KeySet{}, true},
		{"default is optional", "", tampered, trusted, false},
		{"off+bad signature", PolicyOff, tampered, trusted, true},
	}
	for _, c := range cases {
		f := &RemoteFetcher{Inner: fakeFetcher{card: remoteCard(c.raw)}, Policy: c.policy, Keys: c.keys}
		got, err := f.FetchCard(context.Background(), "https://remote.example")
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok=%v", c.name, err, c.ok)
		}
		if err == nil && c.name == "required+valid" && (!got.Verified || got.SignerKeyID != s.KeyID()) {
			t.Errorf("%s: card not marked verified: %+v", c.name, got)
		}
	}
	// Required with no bytes to verify is a refusal.
	f := &RemoteFetcher{Inner: fakeFetcher{card: &domain.AgentCard{Name: "x"}}, Policy: PolicyRequired, Keys: trusted}
	if _, err := f.FetchCard(context.Background(), "e"); err == nil {
		t.Error("required + no raw bytes accepted")
	}
}

// ── publishing ───────────────────────────────────────────────────────────────

type fakeRegistry struct {
	defs []domain.AgentDefinition
	mans map[string]*domain.AgentManifest
}

func (r fakeRegistry) GetAllAgents(context.Context) ([]domain.AgentDefinition, error) {
	return r.defs, nil
}

func (r fakeRegistry) GetManifest(_ context.Context, id string) (*domain.AgentManifest, error) {
	if m, ok := r.mans[id]; ok {
		return m, nil
	}
	return &domain.AgentManifest{}, nil
}

func TestPublisherHandler(t *testing.T) {
	def, man := profileAgent(t, goldenProfile)
	reg := fakeRegistry{
		defs: []domain.AgentDefinition{
			def,
			{ID: "remote", Runtime: domain.RuntimeA2A, A2AEndpoint: "https://r"},
			{ID: "gen", Trait: domain.TraitModel},
			{ID: "organ", System: true},
		},
		mans: map[string]*domain.AgentManifest{def.ID: man},
	}
	s := testSigner(t)
	p := &Publisher{Source: reg, Signer: s, Options: testOptions(), BaseURL: "https://cards.example.org"}
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	get := func(path string) (int, []byte) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return resp.StatusCode, buf.Bytes()
	}

	code, body := get("/a2a/researcher" + WellKnownPath)
	if code != http.StatusOK {
		t.Fatalf("card: %d %s", code, body)
	}
	_, jwks := get("/.well-known/jwks.json")
	ks, err := ParseJWKS(jwks)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(body, ks); err != nil {
		t.Fatalf("served card does not verify against the served JWKS: %v", err)
	}
	for _, path := range []string{"/a2a/remote" + WellKnownPath, "/a2a/gen" + WellKnownPath,
		"/a2a/organ" + WellKnownPath, "/a2a/nobody" + WellKnownPath, "/a2a/researcher/x" + WellKnownPath} {
		if code, _ := get(path); code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, code)
		}
	}
	_, idx := get("/a2a/agents")
	if string(idx) != `{"agents":[{"id":"researcher","card_url":"https://cards.example.org/a2a/researcher/.well-known/agent-card.json"}]}` {
		t.Errorf("index = %s", idx)
	}
	resp, err := http.Post(srv.URL+"/a2a/agents", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", resp.StatusCode)
	}

	// System agents are published only when asked.
	p.IncludeSystem = true
	cards, err := p.Cards(context.Background())
	if err != nil || len(cards) != 2 {
		t.Fatalf("cards with system: %d %v", len(cards), err)
	}
}

// ADR-0139 amendment 2026-11-06: an agent that pushes its own live step output
// says so on its card's Cambrian extension; a default (false) is never emitted.
func TestBuild_StreamsOutputParam(t *testing.T) {
	def := domain.AgentDefinition{ID: "ext", Name: "External", Description: "d"}
	if _, ok := Build(def, &domain.AgentManifest{}, BuildOptions{}).Capabilities.Extensions[0].Params["streams_output"]; ok {
		t.Fatal("streams_output=false must not be emitted (A2A §8.4.1 default-valued member)")
	}
	got := Build(def, &domain.AgentManifest{StreamsOutput: true}, BuildOptions{}).Capabilities.Extensions[0].Params["streams_output"]
	if got != true {
		t.Fatalf("streams_output param = %v, want true", got)
	}
}
