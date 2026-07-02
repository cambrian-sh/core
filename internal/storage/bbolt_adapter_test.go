package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

// Test 1: Same file content + same version → same SourceHash
func TestComputeSourceHash_SameInputsSameHash(t *testing.T) {
	content := []byte("print('hello world')")
	version := "1.0.0"

	h1 := ComputeSourceHash(version, content)
	h2 := ComputeSourceHash(version, content)

	if h1 != h2 {
		t.Fatalf("expected identical hashes for identical inputs, got %q and %q", h1, h2)
	}
	if h1 == "" {
		t.Fatal("expected non-empty hash")
	}
}

// Test 2: Different file content, same version → different SourceHash
func TestComputeSourceHash_DifferentContentDifferentHash(t *testing.T) {
	version := "1.0.0"
	h1 := ComputeSourceHash(version, []byte("print('hello')"))
	h2 := ComputeSourceHash(version, []byte("print('world')"))

	if h1 == h2 {
		t.Fatalf("expected different hashes for different file content, both got %q", h1)
	}
}

// Test 3: Same file content, different version → different SourceHash
func TestComputeSourceHash_DifferentVersionDifferentHash(t *testing.T) {
	content := []byte("print('hello world')")
	h1 := ComputeSourceHash("1.0.0", content)
	h2 := ComputeSourceHash("2.0.0", content)

	if h1 == h2 {
		t.Fatalf("expected different hashes for different manifest versions, both got %q", h1)
	}
}

// ── Cycle 5: BBoltAdapter.GetManifest roundtrips Dependencies ────────────────

func newTestAdapter(t *testing.T) (*BBoltAdapter, func()) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := bbolt.Open(dbPath, 0600, &bbolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		t.Fatalf("open bbolt: %v", err)
	}
	_ = db.Update(func(tx *bbolt.Tx) error {
		_, _ = tx.CreateBucketIfNotExists(manifestBucket)
		return nil
	})
	adapter := &BBoltAdapter{db: db}
	return adapter, func() {
		db.Close()
		os.RemoveAll(dir)
	}
}

func TestGetManifest_Dependencies_RoundTrip(t *testing.T) {
	adapter, cleanup := newTestAdapter(t)
	defer cleanup()

	manifest := ManifestRecord{
		Version:      "1.0.0",
		Tools:        []string{"sql"},
		Dependencies: []string{"pandas>=2.0", "numpy"},
	}
	data, _ := json.Marshal(manifest)
	_ = adapter.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(manifestBucket).Put([]byte("sql-agent"), data)
	})

	got, err := adapter.GetManifestRecord("sql-agent")
	if err != nil {
		t.Fatalf("GetManifestRecord: %v", err)
	}
	if len(got.Dependencies) != 2 {
		t.Fatalf("want 2 dependencies, got %d: %v", len(got.Dependencies), got.Dependencies)
	}
	if got.Dependencies[0] != "pandas>=2.0" {
		t.Errorf("Dependencies[0]: want pandas>=2.0, got %q", got.Dependencies[0])
	}
	if got.Dependencies[1] != "numpy" {
		t.Errorf("Dependencies[1]: want numpy, got %q", got.Dependencies[1])
	}
}

func TestGetManifest_AbsentAgent_ReturnsEmptyManifest(t *testing.T) {
	adapter, cleanup := newTestAdapter(t)
	defer cleanup()

	got, err := adapter.GetManifestRecord("nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil empty manifest")
	}
	if len(got.Dependencies) != 0 {
		t.Errorf("want empty dependencies for absent agent, got %v", got.Dependencies)
	}
}

// Test 4: Empty version string → stable hash (no panic)
func TestComputeSourceHash_EmptyVersionStable(t *testing.T) {
	content := []byte("print('hello world')")

	// Must not panic
	h1 := ComputeSourceHash("", content)
	h2 := ComputeSourceHash("", content)

	if h1 != h2 {
		t.Fatalf("expected stable hash for empty version, got %q and %q", h1, h2)
	}
	if h1 == "" {
		t.Fatal("expected non-empty hash even with empty version")
	}
}
