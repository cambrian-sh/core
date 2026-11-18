package storage

import (
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

// ENG-2019: the never-wired ADR-0021 retrieval-session log is deleted, so a new store
// does not create the retrieval_sessions bucket (no store can hold query text outside
// ADR-0137 erasure), and a store an earlier build created — bucket present, even with
// a record in it — still opens through the production opener, its bucket left as is.
func TestRetrievalSessionsBucket_RetiredNotCreatedOldStoreOpens(t *testing.T) {
	dir := t.TempDir()

	fresh, err := NewBBoltAdapter(filepath.Join(dir, "fresh.db"), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewBBoltAdapter fresh: %v", err)
	}
	if err := fresh.db.View(func(tx *bbolt.Tx) error {
		if tx.Bucket([]byte("retrieval_sessions")) != nil {
			t.Error("a new store created the retired retrieval_sessions bucket")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fresh.Close()

	oldPath := filepath.Join(dir, "old.db")
	raw, err := bbolt.Open(oldPath, 0o600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucket([]byte("retrieval_sessions"))
		if err != nil {
			return err
		}
		return b.Put([]byte("ret-1"), []byte(`{"session_id":"ret-1","query":"q"}`))
	}); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	old, err := NewBBoltAdapter(oldPath, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewBBoltAdapter on a store with the retired bucket: %v", err)
	}
	defer old.Close()
	if err := old.WriteAgentRecord(AgentRecord{ID: "agent:a", Trait: "model"}); err != nil {
		t.Fatalf("store with the retired bucket is not writable: %v", err)
	}
	if err := old.db.View(func(tx *bbolt.Tx) error {
		if b := tx.Bucket([]byte("retrieval_sessions")); b == nil || b.Get([]byte("ret-1")) == nil {
			t.Error("opening an old store changed its retired bucket; it must be left as is")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
