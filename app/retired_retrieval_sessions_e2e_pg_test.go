package app

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

// ENG-2052 and ENG-2069 through the real boot path: a kernel whose agents.db an
// earlier build wrote — with the retired ADR-0021 retrieval_sessions bucket holding
// a query and the retired traversal_log bucket holding an entry — boots
// (kernel.BuildStorage → storage.NewBBoltAdapterNoScan) and both buckets are gone
// afterwards.
func TestPg_BootDropsTheRetiredRetrievalSessionsBucket(t *testing.T) {
	cfg := retrievalScratchConfig(t)
	if err := os.MkdirAll(cfg.Storage.DataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(cfg.Storage.DataDir, cfg.Storage.DBName)
	raw, err := bbolt.Open(dbPath, 0o600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucket([]byte("retrieval_sessions"))
		if err != nil {
			return err
		}
		if err := b.Put([]byte("ret-1"), []byte(`{"session_id":"ret-1","query":"what did alice say"}`)); err != nil {
			return err
		}
		tl, err := tx.CreateBucket([]byte("traversal_log"))
		if err != nil {
			return err
		}
		return tl.Put([]byte("trav-a-b-1"), []byte(`{"entry_id":"trav-a-b-1","source_id":"a","target_id":"b"}`))
	}); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	k, err := bootstrapKernel(ctx, cfg, lis, Options{}, nil)
	if err != nil {
		t.Fatalf("bootstrapKernel on a store with the retired bucket: %v", err)
	}
	shutdownWithin(t, k, 30*time.Second)

	after, err := bbolt.Open(dbPath, 0o600, &bbolt.Options{Timeout: 5 * time.Second, ReadOnly: true})
	if err != nil {
		t.Fatalf("reopen agents.db after shutdown: %v", err)
	}
	defer after.Close()
	if err := after.View(func(tx *bbolt.Tx) error {
		if tx.Bucket([]byte("retrieval_sessions")) != nil {
			t.Error("the retired retrieval_sessions bucket survived the boot")
		}
		if tx.Bucket([]byte("traversal_log")) != nil {
			t.Error("the retired traversal_log bucket survived the boot")
		}
		if tx.Bucket([]byte("agents")) == nil {
			t.Error("the boot left the store without its live buckets")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
