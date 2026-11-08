package storage

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

// ADR-0141: provider-owned sealed records are encrypted at rest, confined to
// their namespace, and enumerable by key only within it.
func TestSealedRecords_NamespacedAndEncrypted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.db")
	s, err := OpenConfigStore(path)
	if err != nil {
		t.Fatal(err)
	}
	acc, err := s.SealedRecords("accounts")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.SealedRecords("other")
	if err != nil {
		t.Fatal(err)
	}
	const plain = "refresh-token-PLAINTEXT-77aa"
	if err := acc.PutSealed("gmail\x1falice", []byte(plain)); err != nil {
		t.Fatal(err)
	}
	if err := other.PutSealed("x", []byte("y")); err != nil {
		t.Fatal(err)
	}
	got, ok, err := acc.GetSealed("gmail\x1falice")
	if err != nil || !ok || string(got) != plain {
		t.Fatalf("get = %q %v %v", got, ok, err)
	}
	if _, ok, _ := other.GetSealed("gmail\x1falice"); ok {
		t.Fatal("a namespace must not see another namespace's record")
	}
	keys, err := acc.SealedKeys()
	if err != nil || len(keys) != 1 || keys[0] != "gmail\x1falice" {
		t.Fatalf("keys = %q %v", keys, err)
	}
	for _, bad := range []string{"", "a/b"} {
		if _, err := s.SealedRecords(bad); err == nil {
			t.Fatalf("namespace %q must be refused", bad)
		}
	}
	existed, err := acc.DeleteSealed("gmail\x1falice")
	if err != nil || !existed {
		t.Fatalf("delete = %v %v", existed, err)
	}
	if err := acc.PutSealed("k", []byte(plain)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(plain)) {
		t.Fatal("sealed record plaintext found in the store file")
	}
}

// ADR-0141 migration: ADR-0140's user_credentials bucket is moved into the
// "accounts" namespace on open (bytes unchanged, same key), and the legacy
// bucket is deleted, so already-connected people keep their accounts.
func TestSealedRecords_MigratesADR0140Bucket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.db")
	s, err := OpenConfigStore(path)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := s.seal([]byte(`{"integration_id":"slack"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(legacyUserCredentialBucket)
		if err != nil {
			return err
		}
		return b.Put([]byte("slack\x1fbob"), sealed)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = OpenConfigStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	acc, _ := s.SealedRecords(LegacyAccountsNamespace)
	got, ok, err := acc.GetSealed("slack\x1fbob")
	if err != nil || !ok || string(got) != `{"integration_id":"slack"}` {
		t.Fatalf("migrated record = %q %v %v", got, ok, err)
	}
	if err := s.db.View(func(tx *bbolt.Tx) error {
		if tx.Bucket(legacyUserCredentialBucket) != nil {
			t.Error("legacy user_credentials bucket must be deleted after migration")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
