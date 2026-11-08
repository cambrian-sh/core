package storage

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Integrations: secrets are encrypted at rest (the plaintext never appears in
// the store file) and carry a last-updated time that is readable without the
// value; clearing removes both.
func TestConfigStore_SecretEncryptedAtRestWithUpdatedAt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.db")
	s, err := OpenConfigStore(path)
	if err != nil {
		t.Fatal(err)
	}
	const plain = "postgres-password-PLAINTEXT-9f3c"
	before := time.Now().Add(-time.Second)
	if err := s.SetSecret("mcp:pg:env:POSTGRES_PASSWORD", plain); err != nil {
		t.Fatal(err)
	}
	at := s.SecretUpdatedAt("mcp:pg:env:POSTGRES_PASSWORD")
	if at.Before(before) || at.After(time.Now().Add(time.Second)) {
		t.Fatalf("updated_at = %v", at)
	}
	if !s.SecretUpdatedAt("mcp:pg:env:NOPE").IsZero() {
		t.Fatal("an absent secret must report zero time")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(plain)) {
		t.Fatal("SECRET LEAK: plaintext found in the store file")
	}
	s, err = OpenConfigStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Resolve("mcp:pg:env:POSTGRES_PASSWORD", "") != plain {
		t.Fatal("round trip failed")
	}
	if err := s.ClearSecret("mcp:pg:env:POSTGRES_PASSWORD"); err != nil {
		t.Fatal(err)
	}
	if s.Configured("mcp:pg:env:POSTGRES_PASSWORD") || !s.SecretUpdatedAt("mcp:pg:env:POSTGRES_PASSWORD").IsZero() {
		t.Fatal("clear left the secret or its timestamp")
	}
}
