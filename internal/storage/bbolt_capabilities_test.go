package storage

import (
	"path/filepath"
	"testing"
)

// newCapabilityTestAdapter opens a fully-initialized BBoltAdapter backed by a
// temp DB and an empty agents dir. All buckets (including capability_clusters)
// are created by Seed; no agents are auto-seeded.
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

// TestSetClusterName_GetClusterName_RoundTrip verifies that a cluster name
// written via SetClusterName is returned intact by GetClusterName.
func TestSetClusterName_GetClusterName_RoundTrip(t *testing.T) {
	adapter, cleanup := newCapabilityTestAdapter(t)
	defer cleanup()

	if err := adapter.SetClusterName("agent-rep-1", "vision-processing"); err != nil {
		t.Fatalf("SetClusterName: %v", err)
	}

	name, err := adapter.GetClusterName("agent-rep-1")
	if err != nil {
		t.Fatalf("GetClusterName: %v", err)
	}
	if name != "vision-processing" {
		t.Errorf("GetClusterName: got %q, want %q", name, "vision-processing")
	}
}

// TestGetClusterName_AbsentKey returns empty string without error.
func TestGetClusterName_AbsentKey(t *testing.T) {
	adapter, cleanup := newCapabilityTestAdapter(t)
	defer cleanup()

	name, err := adapter.GetClusterName("nonexistent-rep")
	if err != nil {
		t.Fatalf("expected nil error for absent key, got: %v", err)
	}
	if name != "" {
		t.Errorf("expected empty string for absent key, got %q", name)
	}
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
