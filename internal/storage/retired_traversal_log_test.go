package storage

import (
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

// ENG-2069 deleted the never-wired ADR-0021 traversal log (SpreadingEngine's
// TraversalLogger was assigned nowhere and nothing read the log), so a new store
// does not create the traversal_log bucket, and a store an earlier build created —
// every one had it, empty, because it was created at each open — has it dropped
// when either production opener opens it. The store stays writable; a second open
// is a no-op.

func hasTraversalLog(t *testing.T, db *bbolt.DB) bool {
	t.Helper()
	found := false
	if err := db.View(func(tx *bbolt.Tx) error {
		found = tx.Bucket(retiredTraversalLogBucket) != nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return found
}

func oldStoreWithTraversalLog(t *testing.T, withRecord bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucket(retiredTraversalLogBucket)
		if err != nil {
			return err
		}
		if withRecord {
			return b.Put([]byte("trav-a-b-1"), []byte(`{"entry_id":"trav-a-b-1","source_id":"a","target_id":"b"}`))
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

func TestTraversalLogBucket_NotCreatedOnAFreshStore(t *testing.T) {
	fresh, err := NewBBoltAdapter(filepath.Join(t.TempDir(), "fresh.db"), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewBBoltAdapter fresh: %v", err)
	}
	defer fresh.Close()
	if hasTraversalLog(t, fresh.db) {
		t.Error("a new store created the retired traversal_log bucket")
	}
}

func TestTraversalLogBucket_DroppedWhenAnOldStoreOpens(t *testing.T) {
	openers := map[string]func(path string) (*BBoltAdapter, error){
		"NewBBoltAdapter":       func(p string) (*BBoltAdapter, error) { return NewBBoltAdapter(p, t.TempDir(), nil) },
		"NewBBoltAdapterNoScan": NewBBoltAdapterNoScan,
	}
	for name, open := range openers {
		for _, withRecord := range []bool{false, true} {
			t.Run(name+map[bool]string{false: "/empty", true: "/with-record"}[withRecord], func(t *testing.T) {
				path := oldStoreWithTraversalLog(t, withRecord)

				a, err := open(path)
				if err != nil {
					t.Fatalf("open a store with the retired bucket: %v", err)
				}
				if hasTraversalLog(t, a.db) {
					t.Fatal("the retired traversal_log bucket survived the open")
				}
				if err := a.WriteAgentRecord(AgentRecord{ID: "agent:a", Trait: "model"}); err != nil {
					t.Fatalf("store not writable after the drop: %v", err)
				}
				if err := a.Close(); err != nil {
					t.Fatal(err)
				}

				b, err := open(path)
				if err != nil {
					t.Fatalf("second open: %v", err)
				}
				defer b.Close()
				if hasTraversalLog(t, b.db) {
					t.Fatal("the retired bucket came back")
				}
				if rec, err := b.GetAgentRecord("agent:a"); err != nil || rec == nil {
					t.Fatalf("agent written before the second open is gone: %v, %v", rec, err)
				}
			})
		}
	}
}
