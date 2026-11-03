package storage

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
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
	// A plan-scoped type: this test is about filesystem routing and GC deleting
	// the blob file. Durable types are never evicted by GC
	// (TestContentStore_GC_NeverEvictsDurableNodeTypes).
	cid, err := cs.Put(bg, large, domain.NodeTypeStepResult, []string{"large"}, "")
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

// T-20261103-plan-scoped-contentstore-gc-evicts-evidence: the store is shared by
// plan scratch and durable writers (evidence ADR-0105, source documents ADR-0060
// D8, memory descriptors, tool artifacts). A plan's GC must evict only scratch.
func TestContentStore_GC_NeverEvictsDurableNodeTypes(t *testing.T) {
	cs := newTestContentStore(t)
	durable := map[string]domain.CID{}
	for _, typ := range []string{"evidence", "source_document", "ltm_fact", "tool_artifact", "agent_artifact", "some_future_type"} {
		cid, err := cs.Put(bg, []byte("durable bytes of "+typ), typ, nil, "")
		if err != nil {
			t.Fatalf("Put %s: %v", typ, err)
		}
		durable[typ] = cid
	}
	step, _ := cs.Put(bg, []byte("scratch step"), domain.NodeTypeStepResult, nil, "")
	tool, _ := cs.Put(bg, []byte("scratch tool"), domain.NodeTypeToolResult, nil, "")
	big := bytes.Repeat([]byte("e"), 64*1024) // filesystem-routed durable blob
	bigCID, _ := cs.Put(bg, big, "evidence", nil, "")

	if err := cs.GC(bg, nil); err != nil { // the worst case: a plan that kept nothing
		t.Fatalf("GC: %v", err)
	}
	for typ, cid := range durable {
		if _, err := cs.Get(bg, cid); err != nil {
			t.Errorf("durable %s node evicted by plan GC: %v", typ, err)
		}
	}
	if _, err := cs.Get(bg, bigCID); err != nil {
		t.Errorf("filesystem-routed evidence blob evicted by plan GC: %v", err)
	}
	for name, cid := range map[string]domain.CID{"step_result": step, "tool_result": tool} {
		if _, err := cs.Get(bg, cid); err == nil {
			t.Errorf("plan-scoped %s node should be evicted", name)
		}
	}
}

// A durable writer storing bytes a plan already stored as scratch must not lose
// them to the next GC through the dedup.
func TestContentStore_Put_DedupPromotesScratchToDurable(t *testing.T) {
	cs := newTestContentStore(t)
	data := []byte("same bytes, first as a step result, then as evidence")
	cid1, _ := cs.Put(bg, data, domain.NodeTypeStepResult, nil, "")
	cid2, err := cs.Put(bg, data, "evidence", nil, "")
	if err != nil || cid1 != cid2 {
		t.Fatalf("dedup Put: cid1=%s cid2=%s err=%v", cid1, cid2, err)
	}
	node, _ := cs.Get(bg, cid1)
	if node.Type != "evidence" {
		t.Errorf("type after durable dedup = %q, want evidence", node.Type)
	}
	if err := cs.GC(bg, nil); err != nil {
		t.Fatalf("GC: %v", err)
	}
	if _, err := cs.Get(bg, cid1); err != nil {
		t.Errorf("promoted node evicted by GC: %v", err)
	}
	// The reverse never demotes: scratch after durable stays durable.
	cid3, _ := cs.Put(bg, []byte("durable first"), "source_document", nil, "")
	_, _ = cs.Put(bg, []byte("durable first"), domain.NodeTypeStepResult, nil, "")
	if n, _ := cs.Get(bg, cid3); n.Type != "source_document" {
		t.Errorf("scratch Put demoted a durable node to %q", n.Type)
	}
}
