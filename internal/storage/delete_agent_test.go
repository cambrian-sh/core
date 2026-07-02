package storage

import (
	"path/filepath"
	"testing"
)

func newAgentDeleteTestAdapter(t *testing.T) (*BBoltAdapter, func()) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "del_test.db")
	adapter, err := NewBBoltAdapter(dbPath, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewBBoltAdapter: %v", err)
	}
	return adapter, func() { adapter.Close() }
}

// DeleteAgentRecord removes the agent so a subsequent read returns not-found,
// and leaves a sibling agent intact (the reconcile deletes a computed orphan
// set, not the whole bucket).
func TestBBoltAdapter_DeleteAgentRecord_RemovesOnlyTarget(t *testing.T) {
	adapter, cleanup := newAgentDeleteTestAdapter(t)
	defer cleanup()

	if err := adapter.WriteAgentRecord(AgentRecord{ID: "llm:ollama:qwen3:8b", Trait: "model"}); err != nil {
		t.Fatalf("WriteAgentRecord orphan: %v", err)
	}
	if err := adapter.WriteAgentRecord(AgentRecord{ID: "llm:deepseek", Trait: "model"}); err != nil {
		t.Fatalf("WriteAgentRecord keeper: %v", err)
	}

	if err := adapter.DeleteAgentRecord("llm:ollama:qwen3:8b"); err != nil {
		t.Fatalf("DeleteAgentRecord: %v", err)
	}

	if rec, _ := adapter.GetAgentRecord("llm:ollama:qwen3:8b"); rec != nil {
		t.Errorf("expected orphan gone, still present: %+v", rec)
	}
	rec, err := adapter.GetAgentRecord("llm:deepseek")
	if err != nil || rec == nil {
		t.Errorf("expected keeper to survive, got rec=%v err=%v", rec, err)
	}
}

// DeleteAgentRecord is idempotent — deleting an absent id is not an error, so the
// reconcile can prune a set without a prior existence check.
func TestBBoltAdapter_DeleteAgentRecord_IdempotentOnAbsent(t *testing.T) {
	adapter, cleanup := newAgentDeleteTestAdapter(t)
	defer cleanup()

	if err := adapter.DeleteAgentRecord("never-existed"); err != nil {
		t.Errorf("expected nil error deleting absent id, got %v", err)
	}
}
