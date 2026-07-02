package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"

	"go.etcd.io/bbolt"
)

func newTestStepCache(t *testing.T) (*BBoltStepCache, func()) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "step_cache_test.db")
	db, err := bbolt.Open(dbPath, 0600, &bbolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		t.Fatalf("open bbolt: %v", err)
	}
	cache, err := NewBBoltStepCache(db)
	if err != nil {
		db.Close()
		t.Fatalf("NewBBoltStepCache: %v", err)
	}
	return cache, func() {
		db.Close()
		os.RemoveAll(dir)
	}
}

// Cycle 1 — tracer bullet: Put then Get returns the same Handoff.
func TestBBoltStepCache_PutAndGet_RoundTrip(t *testing.T) {
	cache, cleanup := newTestStepCache(t)
	defer cleanup()

	original := &domain.Handoff{
		ID:        "h1",
		FromAgent: "agent-a",
		Payload:   &domain.Payload{Data: []byte("hello world"), Type: "text"},
		Context:   map[string]string{"plan": "q3-report", "step": "3"},
	}

	if err := cache.Put(context.Background(), "key1", original, time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, ok, err := cache.Get(context.Background(), "key1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("expected cache hit, got miss")
	}
	if string(got.Payload.Data) != "hello world" {
		t.Errorf("payload mismatch: got %q", got.Payload.Data)
	}
	if got.Context["plan"] != "q3-report" {
		t.Errorf("context key 'plan' mismatch: got %v", got.Context)
	}
	if got.FromAgent != "agent-a" {
		t.Errorf("FromAgent mismatch: got %q", got.FromAgent)
	}
}

// Cycle 2 — missing key returns (nil, false, nil).
func TestBBoltStepCache_GetMiss_ReturnsNilFalseNilError(t *testing.T) {
	cache, cleanup := newTestStepCache(t)
	defer cleanup()

	got, ok, err := cache.Get(context.Background(), "nonexistent")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok || got != nil {
		t.Fatalf("expected miss, got hit: ok=%v handoff=%+v", ok, got)
	}
}

// Cycle 3 — expired entry returns miss.
func TestBBoltStepCache_TTLExpiration_ReturnsMiss(t *testing.T) {
	cache, cleanup := newTestStepCache(t)
	defer cleanup()

	// Clock is 2h in the past so ExpiresAt = past+1h = 1h ago.
	past := time.Now().Add(-2 * time.Hour)
	cache.nowFn = func() time.Time { return past }

	h := &domain.Handoff{Payload: &domain.Payload{Data: []byte("stale data")}}
	if err := cache.Put(context.Background(), "stale_key", h, time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}
	cache.nowFn = time.Now // restore

	got, ok, err := cache.Get(context.Background(), "stale_key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok || got != nil {
		t.Fatal("expected miss for expired entry")
	}
}

// Cycle 4 — expired entry is deleted from the bucket on read.
func TestBBoltStepCache_ExpiredEntry_DeletedOnRead(t *testing.T) {
	cache, cleanup := newTestStepCache(t)
	defer cleanup()

	past := time.Now().Add(-2 * time.Hour)
	cache.nowFn = func() time.Time { return past }
	h := &domain.Handoff{Payload: &domain.Payload{Data: []byte("gone")}}
	_ = cache.Put(context.Background(), "del_key", h, time.Hour)
	cache.nowFn = time.Now

	_, _, _ = cache.Get(context.Background(), "del_key") // triggers delete

	var found bool
	_ = cache.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(stepCacheBucket)
		found = b != nil && b.Get([]byte("del_key")) != nil
		return nil
	})
	if found {
		t.Fatal("expected expired entry to be physically deleted from bucket after Get")
	}
}

// Cycle 5 — empty key is a no-op for both Put and Get.
func TestBBoltStepCache_EmptyKey_Noop(t *testing.T) {
	cache, cleanup := newTestStepCache(t)
	defer cleanup()

	h := &domain.Handoff{Payload: &domain.Payload{Data: []byte("x")}}
	if err := cache.Put(context.Background(), "", h, time.Hour); err != nil {
		t.Fatalf("Put with empty key returned error: %v", err)
	}
	got, ok, err := cache.Get(context.Background(), "")
	if err != nil || ok || got != nil {
		t.Fatalf("expected no-op for empty key: ok=%v err=%v handoff=%+v", ok, err, got)
	}
}
