package storage

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// newTestContentStore creates a BBoltContentStore backed by a temp file.
func newTestContentStore(t *testing.T) *BBoltContentStore {
	t.Helper()
	dir := t.TempDir()
	cs, err := NewBBoltContentStore(filepath.Join(dir, "cs_test.db"), filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("NewBBoltContentStore: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

var bg = context.Background()

func TestContentStore_Put_ReturnsCID(t *testing.T) {
	cs := newTestContentStore(t)
	cid, err := cs.Put(bg, []byte("hello world"), "step_result", []string{"step_0"}, "")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if cid == "" {
		t.Fatal("expected non-empty CID")
	}
}

func TestContentStore_Put_SameContentSameCID(t *testing.T) {
	cs := newTestContentStore(t)
	data := []byte("deterministic content")
	cid1, _ := cs.Put(bg, data, "step_result", nil, "")
	cid2, _ := cs.Put(bg, data, "step_result", nil, "")
	if cid1 != cid2 {
		t.Errorf("same content produced different CIDs: %q vs %q", cid1, cid2)
	}
}

func TestContentStore_Get_ReturnsOriginalData(t *testing.T) {
	cs := newTestContentStore(t)
	want := []byte("step result content for round-trip test")
	cid, err := cs.Put(bg, want, "step_result", []string{"step_1", "result"}, "inline snippet")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	node, err := cs.Get(bg, cid)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(node.Data, want) {
		t.Errorf("data mismatch: got %q want %q", node.Data, want)
	}
	if node.CID != cid {
		t.Errorf("node.CID mismatch: got %q want %q", node.CID, cid)
	}
	if node.Type != "step_result" {
		t.Errorf("node.Type: got %q want %q", node.Type, "step_result")
	}
	if node.Snippet != "inline snippet" {
		t.Errorf("node.Snippet: got %q want %q", node.Snippet, "inline snippet")
	}
}

func TestContentStore_Has_FalseForUnknownCID(t *testing.T) {
	cs := newTestContentStore(t)
	ok, err := cs.Has(bg, "nonexistent-cid-abc123")
	if err != nil {
		t.Fatalf("Has: %v", err)
	}
	if ok {
		t.Error("Has must return false for unknown CID")
	}
}

func TestContentStore_Has_TrueAfterPut(t *testing.T) {
	cs := newTestContentStore(t)
	cid, _ := cs.Put(bg, []byte("some data"), "ltm_doc", nil, "")
	ok, err := cs.Has(bg, cid)
	if err != nil {
		t.Fatalf("Has: %v", err)
	}
	if !ok {
		t.Error("Has must return true after Put")
	}
}

func TestContentStore_Put_DeduplicatesIdenticalContent(t *testing.T) {
	cs := newTestContentStore(t)
	data := []byte("deduplicated content")
	cid1, _ := cs.Put(bg, data, "step_result", []string{"step_2"}, "")
	cid2, _ := cs.Put(bg, data, "step_result", []string{"step_3"}, "") // different labels
	if cid1 != cid2 {
		t.Errorf("dedup failed: %q vs %q", cid1, cid2)
	}
	node, err := cs.Get(bg, cid1)
	if err != nil {
		t.Fatalf("Get after dedup: %v", err)
	}
	if !bytes.Equal(node.Data, data) {
		t.Errorf("data corrupted after dedup Put: got %q want %q", node.Data, data)
	}
}

func TestContentStore_GC_KeepsDeclaredCIDs(t *testing.T) {
	cs := newTestContentStore(t)
	keep, _ := cs.Put(bg, []byte("keep me"), "step_result", nil, "")
	evict, _ := cs.Put(bg, []byte("evict me"), "step_result", nil, "")

	if err := cs.GC(bg, []domain.CID{keep}); err != nil {
		t.Fatalf("GC: %v", err)
	}
	if _, err := cs.Get(bg, keep); err != nil {
		t.Errorf("Get of kept CID failed: %v", err)
	}
	if _, err := cs.Get(bg, evict); err == nil {
		t.Error("Get of evicted CID should return error after GC")
	}
}

func TestContentStore_GC_EmptyKeepList_EvictsAll(t *testing.T) {
	cs := newTestContentStore(t)
	cid1, _ := cs.Put(bg, []byte("data one"), "step_result", nil, "")
	cid2, _ := cs.Put(bg, []byte("data two"), "step_result", nil, "")
	if err := cs.GC(bg, nil); err != nil {
		t.Fatalf("GC: %v", err)
	}
	for _, cid := range []domain.CID{cid1, cid2} {
		if _, err := cs.Get(bg, cid); err == nil {
			t.Errorf("CID %q should be evicted", cid)
		}
	}
}

func TestContentStore_GC_NoOp_WhenAllKept(t *testing.T) {
	cs := newTestContentStore(t)
	cid, _ := cs.Put(bg, []byte("keep everything"), "step_result", nil, "")
	if err := cs.GC(bg, []domain.CID{cid}); err != nil {
		t.Fatalf("GC: %v", err)
	}
	if _, err := cs.Get(bg, cid); err != nil {
		t.Errorf("CID should still exist: %v", err)
	}
}

func TestContentStore_LargeData_FilesystemRouting(t *testing.T) {
	cs := newTestContentStore(t)
	large := bytes.Repeat([]byte("x"), 64*1024)
	cid, err := cs.Put(bg, large, "agent_artifact", []string{"large"}, "")
	if err != nil {
		t.Fatalf("Put large: %v", err)
	}
	node, err := cs.Get(bg, cid)
	if err != nil {
		t.Fatalf("Get large: %v", err)
	}
	if !bytes.Equal(node.Data, large) {
		t.Errorf("large data round-trip: got %d bytes, want %d", len(node.Data), len(large))
	}
	if err := cs.GC(bg, nil); err != nil {
		t.Fatalf("GC: %v", err)
	}
	if _, err := cs.Get(bg, cid); err == nil {
		t.Error("large-data CID must be evicted by GC")
	}
	blobPath := filepath.Join(cs.FsDir, string(cid))
	if _, statErr := os.Stat(blobPath); statErr == nil {
		t.Errorf("filesystem blob %q must be deleted by GC", blobPath)
	}
}

func TestContentStore_Put_ConcurrentSameData_NRace(t *testing.T) {
	cs := newTestContentStore(t)
	data := []byte("concurrent dedup test")
	const goroutines = 8
	cids := make([]domain.CID, goroutines)
	errs := make([]error, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := range goroutines {
		go func(idx int) {
			defer wg.Done()
			cids[idx], errs[idx] = cs.Put(bg, data, "step_result", nil, "")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d Put error: %v", i, err)
		}
	}
	first := cids[0]
	for i, cid := range cids {
		if cid != first {
			t.Errorf("goroutine %d returned CID %q, want %q", i, cid, first)
		}
	}
}

// ADR-0048 D4: Put stamps the owning session from ctx; a session-less (system)
// write is ownerless. Get round-trips the owner.
func TestContentStore_OwnerSessionRoundTrip(t *testing.T) {
	cs := newTestContentStore(t)

	owned := domain.WithSessionID(bg, "sess-X")
	cid, err := cs.Put(owned, []byte("agent offload"), "agent_offload", nil, "")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	node, err := cs.Get(bg, cid)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if node.OwnerSession != "sess-X" {
		t.Errorf("OwnerSession = %q, want sess-X", node.OwnerSession)
	}

	cid2, err := cs.Put(bg, []byte("tool result"), "tool_result", nil, "")
	if err != nil {
		t.Fatalf("Put system: %v", err)
	}
	node2, _ := cs.Get(bg, cid2)
	if node2.OwnerSession != "" {
		t.Errorf("system write should be ownerless, got %q", node2.OwnerSession)
	}
}
