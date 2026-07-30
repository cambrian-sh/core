package storage

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/cambrian-sh/core/internal/config"
)

func openTestStore(t *testing.T) (*BoltConfigStore, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.db")
	s, err := OpenConfigStore(path)
	if err != nil {
		t.Fatalf("OpenConfigStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}

// The store must satisfy the ports the config package declares, or LoadConfig
// cannot take it. Compile-time, so a signature drift fails the build rather than
// a test.
var (
	_ config.Store       = (*BoltConfigStore)(nil)
	_ config.SecretStore = (*BoltConfigStore)(nil)
)

func TestConfigStore_OverrideRoundTrip(t *testing.T) {
	s, _ := openTestStore(t)

	if err := s.SetOverride("execution.ewma_alpha", 0.8); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	got, err := s.Overrides()
	if err != nil {
		t.Fatalf("Overrides: %v", err)
	}
	if got["execution.ewma_alpha"] != 0.8 {
		t.Fatalf("got %#v, want 0.8", got["execution.ewma_alpha"])
	}
}

func TestConfigStore_OverridesSurviveReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.db")

	s, err := OpenConfigStore(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.SetOverride("execution.ewma_alpha", 0.8); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	_ = s.Close()

	// The whole point of the store: an operator's edit outlives the process.
	s2, err := OpenConfigStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close() }()

	got, _ := s2.Overrides()
	if got["execution.ewma_alpha"] != 0.8 {
		t.Fatalf("after reopen got %#v, want 0.8", got["execution.ewma_alpha"])
	}
}

func TestConfigStore_DeleteOverrideIsIdempotent(t *testing.T) {
	s, _ := openTestStore(t)

	if err := s.DeleteOverride("never.set"); err != nil {
		t.Fatalf("deleting an absent key must not error: %v", err)
	}
	_ = s.SetOverride("a.b", 1)
	if err := s.DeleteOverride("a.b"); err != nil {
		t.Fatalf("DeleteOverride: %v", err)
	}
	got, _ := s.Overrides()
	if _, present := got["a.b"]; present {
		t.Fatal("key still present after delete")
	}
}

// ── secrets ──────────────────────────────────────────────────────────────────

func TestSecretStore_ConfiguredAndLastFour(t *testing.T) {
	s, _ := openTestStore(t)

	if s.Configured("generator:gpt:api_key") {
		t.Fatal("reported configured before anything was stored")
	}
	if err := s.SetSecret("generator:gpt:api_key", "sk-live-ABCD1234"); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	if !s.Configured("generator:gpt:api_key") {
		t.Fatal("not reported configured after store")
	}
	if got := s.LastFour("generator:gpt:api_key"); got != "1234" {
		t.Fatalf("LastFour = %q, want %q", got, "1234")
	}
}

// A short secret must not return a prefix of itself: for a 3-character value the
// "last four" IS the whole credential.
func TestSecretStore_LastFourRefusesShortSecrets(t *testing.T) {
	s, _ := openTestStore(t)
	_ = s.SetSecret("tiny", "abc")

	if got := s.LastFour("tiny"); got != "" {
		t.Fatalf("LastFour = %q, want \"\" — a 3-char secret would be revealed whole", got)
	}
}

// ADR-0101 D6: the store file alone must be useless. This is the property that
// justifies putting credentials in the data directory at all.
func TestSecretStore_ValueIsNotPlaintextOnDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.db")
	s, err := OpenConfigStore(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	const secret = "sk-live-SUPERSECRET-9999"
	if err := s.SetSecret("generator:gpt:api_key", secret); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	_ = s.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read db: %v", err)
	}
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatal("secret found verbatim in the store file — encryption at rest is not working")
	}
}

// Without the key, the file is genuinely unreadable. This pins the consequence
// ADR-0101 D6 states: lose the key and the secrets are gone, not merely awkward.
func TestSecretStore_UnreadableWithoutTheKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.db")
	s, err := OpenConfigStore(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = s.SetSecret("k", "sk-live-ABCD1234")
	_ = s.Close()

	// Simulate the store travelling without its key file.
	if err := os.Remove(filepath.Join(dir, "secret.key")); err != nil {
		t.Fatalf("remove key: %v", err)
	}
	s2, err := OpenConfigStore(path) // generates a NEW key
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close() }()

	if got := s2.LastFour("k"); got != "" {
		t.Fatalf("LastFour = %q, want \"\" — the old secret must not decrypt under a new key", got)
	}
}

// ADR-0101 D5: the environment wins over the store, so an existing .env-based
// deployment keeps working unchanged after migrating.
func TestSecretStore_ResolvePrefersEnv(t *testing.T) {
	s, _ := openTestStore(t)
	_ = s.SetSecret("generator:gpt:api_key", "from-store")

	t.Setenv("OPENAI_API_KEY", "from-env")
	if got := s.Resolve("generator:gpt:api_key", "OPENAI_API_KEY"); got != "from-env" {
		t.Fatalf("Resolve = %q, want %q", got, "from-env")
	}
}

func TestSecretStore_ResolveFallsBackToStore(t *testing.T) {
	s, _ := openTestStore(t)
	_ = s.SetSecret("generator:gpt:api_key", "from-store")

	// Env var named but unset ⇒ the store answers.
	if got := s.Resolve("generator:gpt:api_key", "DEFINITELY_UNSET_VAR_XYZ"); got != "from-store" {
		t.Fatalf("Resolve = %q, want %q", got, "from-store")
	}
	// No env form at all ⇒ the store answers.
	if got := s.Resolve("generator:gpt:api_key", ""); got != "from-store" {
		t.Fatalf("Resolve = %q, want %q", got, "from-store")
	}
}

func TestSecretStore_ClearIsIdempotent(t *testing.T) {
	s, _ := openTestStore(t)

	if err := s.ClearSecret("never-set"); err != nil {
		t.Fatalf("clearing an absent secret must not error: %v", err)
	}
	_ = s.SetSecret("k", "sk-live-ABCD1234")
	if err := s.ClearSecret("k"); err != nil {
		t.Fatalf("ClearSecret: %v", err)
	}
	if s.Configured("k") {
		t.Fatal("still configured after clear")
	}
}

// The key file must not be world-readable. Checked on POSIX only: Windows does
// not model these bits, and asserting them there would fail for a reason that has
// nothing to do with the code.
func TestSecretStore_KeyFilePermissions(t *testing.T) {
	if os.Getenv("GOOS") == "windows" {
		t.Skip("POSIX permission bits")
	}
	_, dir := openTestStore(t)

	info, err := os.Stat(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatalf("stat key file: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 && os.PathSeparator == '/' {
		t.Fatalf("key file mode = %o, want no group/other access", perm)
	}
}
