package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/agentcard"
	"github.com/cambrian-sh/core/internal/config"
)

// memSecrets is an in-memory ADR-0101 store for the named-secret seam.
type memSecrets struct{ m map[string]string }

func (s *memSecrets) Resolve(name, _ string) string { return s.m[name] }
func (s *memSecrets) SetSecret(name, value string) error {
	s.m[name] = value
	return nil
}
func (s *memSecrets) ClearSecret(name string) error { delete(s.m, name); return nil }
func (s *memSecrets) Configured(name string) bool   { return s.m[name] != "" }
func (s *memSecrets) LastFour(string) string        { return "" }

func withSecretStore(t *testing.T, s namedSecretSource) {
	t.Helper()
	namedSecretMu.Lock()
	prevSrc, prevAdm := namedSecretSrc, namedSecretAdm
	namedSecretMu.Unlock()
	if s == nil {
		namedSecretMu.Lock()
		namedSecretSrc, namedSecretAdm = nil, nil
		namedSecretMu.Unlock()
	} else {
		setNamedSecretSource(s)
	}
	t.Cleanup(func() {
		namedSecretMu.Lock()
		namedSecretSrc, namedSecretAdm = prevSrc, prevAdm
		namedSecretMu.Unlock()
	})
}

func TestLoadCardSigner_KeyFileMintsOnceAndReuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a2a", "card.key")
	a := config.A2AEndpointConfig{SigningKeyFile: path, PublicBaseURL: "https://cards.example.org/"}
	s1, err := loadCardSigner(a)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := loadCardSigner(a)
	if err != nil || s1.KeyID() != s2.KeyID() {
		t.Fatalf("second boot: %v, kid %s vs %s", err, s1.KeyID(), s2.KeyID())
	}
	c, _ := s1.Sign(agentcard.Card{Name: "x"})
	if len(c.Signatures) != 1 {
		t.Fatal("not signed")
	}
}

func TestLoadCardSigner_SecretStoreMintsOnceAndReuses(t *testing.T) {
	store := &memSecrets{m: map[string]string{}}
	withSecretStore(t, store)
	s1, err := loadCardSigner(config.A2AEndpointConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if store.m[A2ASigningKeySecret] == "" {
		t.Fatal("key was not stored in the config store")
	}
	s2, err := loadCardSigner(config.A2AEndpointConfig{})
	if err != nil || s1.KeyID() != s2.KeyID() {
		t.Fatalf("second boot: %v", err)
	}
}

// No key file and no config store: refuse rather than sign with an ephemeral key.
func TestLoadCardSigner_NoStoreNoFileRefuses(t *testing.T) {
	withSecretStore(t, nil)
	if _, err := loadCardSigner(config.A2AEndpointConfig{}); err == nil {
		t.Fatal("want an error with neither a key file nor a config store")
	}
}

// recordingRegistrar registers through the same verifying fetcher boot wires.
type recordingRegistrar struct {
	fetch domain.CardFetcher
	got   []string
}

func (r *recordingRegistrar) RegisterA2AAgent(ctx context.Context, endpoint string) error {
	c, err := r.fetch.FetchCard(ctx, endpoint)
	if err != nil {
		return err
	}
	r.got = append(r.got, c.Name)
	return nil
}

// End to end over HTTP: a remote serves a signed card at the A2A well-known
// path; under signature_policy=required the kernel registers it when the
// signature verifies against the pinned JWKS and refuses it when tampered.
func TestRemoteCards_RequiredPolicyOverHTTP(t *testing.T) {
	signer, err := agentcard.NewSigner(bytes.Repeat([]byte{3}, 32), "")
	if err != nil {
		t.Fatal(err)
	}
	card, err := signer.Sign(agentcard.Build(
		domain.AgentDefinition{ID: "translator", Name: "Translator", Description: "Translates text."},
		&domain.AgentManifest{Version: "2.0.0", Capabilities: []string{"translation"}}, agentcard.BuildOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	good, _ := json.Marshal(card)
	var m map[string]any
	_ = json.Unmarshal(good, &m)
	m["description"] = "Translates text and uploads your files."
	tampered, _ := json.Marshal(m)

	mux := http.NewServeMux()
	mux.HandleFunc("/good"+agentcard.WellKnownPath, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(good) })
	mux.HandleFunc("/bad"+agentcard.WellKnownPath, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(tampered) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	jwks, _ := json.Marshal(signer.JWKS())
	jwksPath := filepath.Join(t.TempDir(), "trusted.jwks.json")
	if err := os.WriteFile(jwksPath, jwks, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.A2A = config.A2AConfig{SignaturePolicy: "required", TrustedJWKSFile: jwksPath}
	fetcher, err := newRemoteCardFetcher(cfg)
	if err != nil {
		t.Fatal(err)
	}
	reg := &recordingRegistrar{fetch: fetcher}
	registerRemoteA2AAgents(context.Background(), reg, []string{srv.URL + "/good", srv.URL + "/bad/"}, "required")
	if len(reg.got) != 1 || reg.got[0] != "Translator" {
		t.Fatalf("registered %v, want only the verified card", reg.got)
	}
	if _, err := fetcher.FetchCard(context.Background(), srv.URL+"/bad"); err == nil ||
		!strings.Contains(err.Error(), "refused") {
		t.Fatalf("tampered card: err = %v", err)
	}
}

func TestNewRemoteCardFetcher_BadTrustFileIsABootError(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.A2A.TrustedJWKSFile = filepath.Join(t.TempDir(), "missing.json")
	if _, err := newRemoteCardFetcher(cfg); err == nil {
		t.Fatal("missing trust file accepted")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte(`{"keys":[{"kty":"RSA"}]}`), 0o600)
	cfg.A2A.TrustedJWKSFile = bad
	if _, err := newRemoteCardFetcher(cfg); err == nil {
		t.Fatal("unsupported key accepted")
	}
}
