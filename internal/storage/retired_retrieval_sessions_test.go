package storage

import (
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

// ENG-2019 deleted the never-wired ADR-0021 retrieval-session log, so a new store
// does not create the retrieval_sessions bucket. ENG-2052 (owner decision
// 2026-11-18): a store an earlier build created has the bucket dropped when it is
// opened, whatever it holds — the only thing it could hold is query text no
// ADR-0137 erasure reaches. Both production openers do it; the store stays
// writable; a second open is a no-op; a fresh store is unaffected.

func hasRetrievalSessions(t *testing.T, db *bbolt.DB) bool {
	t.Helper()
	found := false
	if err := db.View(func(tx *bbolt.Tx) error {
		found = tx.Bucket(retiredRetrievalSessionsBucket) != nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return found
}

// oldStore writes a bbolt file as an earlier build left it: the retired bucket,
// empty or with one record.
func oldStore(t *testing.T, withRecord bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucket(retiredRetrievalSessionsBucket)
		if err != nil {
			return err
		}
		if withRecord {
			return b.Put([]byte("ret-1"), []byte(`{"session_id":"ret-1","query":"what did alice say"}`))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRetrievalSessionsBucket_NotCreatedOnAFreshStore(t *testing.T) {
	fresh, err := NewBBoltAdapter(filepath.Join(t.TempDir(), "fresh.db"), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewBBoltAdapter fresh: %v", err)
	}
	defer fresh.Close()
	if hasRetrievalSessions(t, fresh.db) {
		t.Error("a new store created the retired retrieval_sessions bucket")
	}
	if err := fresh.WriteAgentRecord(AgentRecord{ID: "agent:a", Trait: "model"}); err != nil {
		t.Fatalf("fresh store not writable: %v", err)
	}
}

func TestRetrievalSessionsBucket_DroppedWhenAnOldStoreOpens(t *testing.T) {
	openers := map[string]func(path string) (*BBoltAdapter, error){
		"NewBBoltAdapter":       func(p string) (*BBoltAdapter, error) { return NewBBoltAdapter(p, t.TempDir(), nil) },
		"NewBBoltAdapterNoScan": NewBBoltAdapterNoScan,
	}
	for name, open := range openers {
		for _, withRecord := range []bool{false, true} {
			t.Run(name+map[bool]string{false: "/empty", true: "/with-record"}[withRecord], func(t *testing.T) {
				path := oldStore(t, withRecord)

				a, err := open(path)
				if err != nil {
					t.Fatalf("open a store with the retired bucket: %v", err)
				}
				if hasRetrievalSessions(t, a.db) {
					t.Fatal("the retired retrieval_sessions bucket survived the open")
				}
				if err := a.WriteAgentRecord(AgentRecord{ID: "agent:a", Trait: "model"}); err != nil {
					t.Fatalf("store not writable after the drop: %v", err)
				}
				if err := a.Close(); err != nil {
					t.Fatal(err)
				}

				// Second open: nothing to drop, the store and its data unchanged.
				b, err := open(path)
				if err != nil {
					t.Fatalf("second open: %v", err)
				}
				defer b.Close()
				if hasRetrievalSessions(t, b.db) {
					t.Fatal("the retired bucket came back")
				}
				rec, err := b.GetAgentRecord("agent:a")
				if err != nil || rec == nil {
					t.Fatalf("agent written before the second open is gone: %v, %v", rec, err)
				}
			})
		}
	}
}

// dropRetiredBuckets is a no-op on a store without the retired buckets.
func TestDropRetiredBuckets_IdempotentWithoutTheBuckets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "none.db")
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i := 0; i < 2; i++ {
		if err := dropRetiredBuckets(db); err != nil {
			t.Fatalf("drop #%d without the bucket: %v", i+1, err)
		}
	}
}
