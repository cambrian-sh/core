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

func newBenchStepCache(b *testing.B) (*BBoltStepCache, func()) {
	b.Helper()
	dir := b.TempDir()
	dbPath := filepath.Join(dir, "bench.db")
	db, err := bbolt.Open(dbPath, 0600, &bbolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		b.Fatalf("open bbolt: %v", err)
	}
	cache, err := NewBBoltStepCache(db)
	if err != nil {
		db.Close()
		b.Fatalf("NewBBoltStepCache: %v", err)
	}
	return cache, func() {
		db.Close()
		os.RemoveAll(dir)
	}
}

// BenchmarkStepCache_Get measures cache hit latency.
// Target: p50 < 1ms (ADR-0026 acceptance criterion).
func BenchmarkStepCache_Get(b *testing.B) {
	cache, cleanup := newBenchStepCache(b)
	defer cleanup()

	h := &domain.Handoff{
		FromAgent: "bench-agent",
		Payload:   &domain.Payload{Data: []byte("benchmark payload data")},
		Context:   map[string]string{"k1": "v1", "k2": "v2"},
	}
	const key = "bench-key"
	if err := cache.Put(context.Background(), key, h, 24*time.Hour); err != nil {
		b.Fatalf("Put: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = cache.Get(context.Background(), key)
	}
}
