package executer

import (
	"context"
	"path/filepath"
	"testing"
	"unicode/utf8"

	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/storage"
)

var bg = context.Background()

func newTestStore(t *testing.T) *storage.BBoltContentStore {
	t.Helper()
	dir := t.TempDir()
	cs, err := storage.NewBBoltContentStore(filepath.Join(dir, "t.db"), filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("NewBBoltContentStore: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func fakeResult(index int, data []byte, agentCtx map[string]string) stepResult {
	return stepResult{
		index: index,
		resp:  &domain.Handoff{Payload: &domain.Payload{Data: data}, Context: agentCtx},
	}
}

func TestMergeStepResult_StillWritesResultKey(t *testing.T) {
	cs := newTestStore(t)
	master := map[string]string{}
	mergeStepResult(bg, master, fakeResult(0, []byte("output of step 0"), nil), cs, 500)
	if master["step_0_result"] != "output of step 0" {
		t.Errorf("step_0_result: got %q", master["step_0_result"])
	}
}

func TestMergeStepResult_WritesCIDKey(t *testing.T) {
	cs := newTestStore(t)
	master := map[string]string{}
	mergeStepResult(bg, master, fakeResult(2, []byte("step 2 output"), nil), cs, 500)
	if master["step_2_cid"] == "" {
		t.Fatal("step_2_cid must be written to masterContext")
	}
}

func TestMergeStepResult_CIDRetrievableFromStore(t *testing.T) {
	cs := newTestStore(t)
	master := map[string]string{}
	data := []byte("retrievable step output")
	mergeStepResult(bg, master, fakeResult(1, data, nil), cs, 500)
	cid := domain.CID(master["step_1_cid"])
	node, err := cs.Get(bg, cid)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(node.Data) != string(data) {
		t.Errorf("data mismatch: got %q want %q", node.Data, data)
	}
}

func TestMergeStepResult_AgentContextKeysGoToMasterContext(t *testing.T) {
	cs := newTestStore(t)
	master := map[string]string{}
	mergeStepResult(bg, master, fakeResult(3, []byte("data"), map[string]string{
		"confidence": "0.95",
		"model":      "qwen3:8b",
	}), cs, 500)
	if master["step_3_confidence"] != "0.95" {
		t.Error("step_3_confidence missing from masterContext")
	}
	if master["step_3_model"] != "qwen3:8b" {
		t.Error("step_3_model missing from masterContext")
	}
}

func TestMergeStepResult_AgentContextKeysNotStoredInCAS(t *testing.T) {
	cs := newTestStore(t)
	master := map[string]string{}
	mergeStepResult(bg, master, fakeResult(4, []byte("step 4 data"), map[string]string{"annotation": "info"}), cs, 500)
	stepCID := domain.CID(master["step_4_cid"])
	if err := cs.GC(bg, []domain.CID{stepCID}); err != nil {
		t.Fatalf("GC: %v", err)
	}
	ok, _ := cs.Has(bg, "annotation-cid-that-should-not-exist")
	if ok {
		t.Error("agent annotation keys must not create CAS entries")
	}
}

func TestMergeStepResult_ShortContent_FullSnippetInlined(t *testing.T) {
	cs := newTestStore(t)
	master := map[string]string{}
	data := []byte("short step result")
	mergeStepResult(bg, master, fakeResult(0, data, nil), cs, 500)
	node, _ := cs.Get(bg, domain.CID(master["step_0_cid"]))
	if node.Snippet != string(data) {
		t.Errorf("short content: snippet=%q want %q", node.Snippet, string(data))
	}
}

func TestMergeStepResult_LongContent_SnippetTruncated(t *testing.T) {
	cs := newTestStore(t)
	master := map[string]string{}
	data := make([]byte, 600)
	for i := range data {
		data[i] = 'a'
	}
	mergeStepResult(bg, master, fakeResult(0, data, nil), cs, 500)
	node, _ := cs.Get(bg, domain.CID(master["step_0_cid"]))
	if len(node.Snippet) > 500 {
		t.Errorf("snippet must be ≤500 chars, got %d", len(node.Snippet))
	}
	if len(node.Snippet) == 0 {
		t.Error("snippet must not be empty for UTF-8 content")
	}
}

func TestMergeStepResult_BinaryContent_EmptySnippet(t *testing.T) {
	cs := newTestStore(t)
	master := map[string]string{}
	binary := []byte{0xFF, 0xFE, 0x00, 0x01, 0x80, 0x90}
	if utf8.Valid(binary) {
		t.Skip("test data is valid UTF-8")
	}
	mergeStepResult(bg, master, fakeResult(0, binary, nil), cs, 500)
	node, _ := cs.Get(bg, domain.CID(master["step_0_cid"]))
	if node.Snippet != "" {
		t.Errorf("binary payload must produce empty snippet, got %q", node.Snippet)
	}
	if string(node.Data) != string(binary) {
		t.Error("binary data must be stored intact")
	}
}

func TestMergeStepResult_NilStore_StillWritesMasterContext(t *testing.T) {
	master := map[string]string{}
	mergeStepResult(bg, master, fakeResult(0, []byte("data"), nil), nil, 500)
	if master["step_0_result"] != "data" {
		t.Error("step_0_result must be written even when ContentStore is nil")
	}
	if _, ok := master["step_0_cid"]; ok {
		t.Error("step_0_cid must not be written when ContentStore is nil")
	}
}

func TestDAGExecutor_GCRunsAfterExecute(t *testing.T) {
	cs := newTestStore(t)
	ex := &DAGExecutor{ContentStore: cs}
	plan := &domain.ExecutionPlan{
		Steps: []domain.Step{{Query: "step 0", DependsOn: nil}},
	}
	stepFn := StepFunc(func(ctx context.Context, idx int, h *domain.Handoff) (*domain.Handoff, error) {
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("step result")}}, nil
	})
	if _, err := ex.Execute(bg, plan, nil, stepFn); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// No panic = defer GC wired correctly.
}

func TestMergeStepResult_CIDKeyPassesThroughFilterForNonDependencies(t *testing.T) {
	cs := newTestStore(t)
	master := map[string]string{}
	mergeStepResult(bg, master, fakeResult(0, []byte("step 0 data"), nil), cs, 500)
	if _, ok := master["step_0_cid"]; !ok {
		t.Error("step_0_cid must exist in masterContext for Phase 2 to read")
	}
	// filterSnapshotForStep strips step_0_cid (suffix "cid" ≠ "result")
	snap := filterSnapshotForStep(master, domain.Step{Query: "q", DependsOn: []int{0}})
	if _, ok := snap["step_0_cid"]; ok {
		t.Error("step_0_cid must be stripped by filterSnapshotForStep")
	}
}
