package storage

import (
	"path/filepath"
	"testing"
)

// newCapabilityTestAdapter opens a fully-initialized BBoltAdapter backed by a
// temp DB and an empty agents dir. All buckets are created by Seed; no agents
// are auto-seeded.
func newCapabilityTestAdapter(t *testing.T) (*BBoltAdapter, func()) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "cap_test.db")
	agentsDir := t.TempDir()
	adapter, err := NewBBoltAdapter(dbPath, agentsDir, nil)
	if err != nil {
		t.Fatalf("NewBBoltAdapter: %v", err)
	}
	return adapter, func() { adapter.Close() }
}

// TestSetCapabilities_NonExistentAgent verifies that SetCapabilities returns an
// error when the agent ID is not present in the bucket.
func TestSetCapabilities_NonExistentAgent(t *testing.T) {
	adapter, cleanup := newCapabilityTestAdapter(t)
	defer cleanup()

	err := adapter.SetCapabilities("ghost-agent", []string{"vision"})
	if err == nil {
		t.Fatal("expected error for non-existent agent, got nil")
	}
}

// TestSetCapabilities_PersistsCapabilities verifies that SetCapabilities writes
// the Capabilities field to an existing AgentRecord and the value is visible
// via GetAllAgentRecords.
func TestSetCapabilities_PersistsCapabilities(t *testing.T) {
	adapter, cleanup := newCapabilityTestAdapter(t)
	defer cleanup()

	if err := adapter.WriteAgentRecord(AgentRecord{ID: "agent-1", Name: "Agent One"}); err != nil {
		t.Fatalf("WriteAgentRecord: %v", err)
	}

	if err := adapter.SetCapabilities("agent-1", []string{"vision", "text"}); err != nil {
		t.Fatalf("SetCapabilities: %v", err)
	}

	recs, err := adapter.GetAllAgentRecords()
	if err != nil {
		t.Fatalf("GetAllAgentRecords: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 agent record, got %d", len(recs))
	}
	got := recs[0].Capabilities
	if len(got) != 2 || got[0] != "vision" || got[1] != "text" {
		t.Errorf("Capabilities: got %v, want [vision text]", got)
	}
}
