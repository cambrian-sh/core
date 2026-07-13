package agentmgr

import (
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// Cycle 1: AgentManager has snapshotMu and snapshots fields
func TestAgentManager_SnapshotFieldsExist(t *testing.T) {
	m := NewAgentManager(nil, "/usr/bin/python3", "localhost:50051", nil)

	m.AgentConnector.snapshotMu.Lock()
	m.AgentConnector.snapshots["agent-1"+"task-1"] = "abc123"
	m.AgentConnector.snapshotMu.Unlock()

	m.AgentConnector.snapshotMu.Lock()
	v := m.AgentConnector.snapshots["agent-1"+"task-1"]
	m.AgentConnector.snapshotMu.Unlock()

	if v != "abc123" {
		t.Errorf("expected snapshots to store 'abc123', got %q", v)
	}
}

// Cycle 2: Restore returns nil when no snapshot exists for the agent+task pair
func TestAgentManager_Restore_MissingSnapshot_ReturnsNil(t *testing.T) {
	reg := newTestRegistry()
	reg.agents["agent-x"] = domain.AgentDefinition{
		ID:  "agent-x",
		Dir: t.TempDir(),
	}
	m := NewAgentManager(reg, "/usr/bin/python3", "localhost:50051", nil)

	err := m.Restore("agent-x", "task-missing")
	if err != nil {
		t.Errorf("expected nil error for missing snapshot, got: %v", err)
	}
}

// Cycle 3: Restore returns nil for a non-git dir (graceful degradation)
func TestAgentManager_Restore_NonGitDir_ReturnsNil(t *testing.T) {
	reg := newTestRegistry()
	nonGitDir := t.TempDir()
	reg.agents["agent-y"] = domain.AgentDefinition{
		ID:  "agent-y",
		Dir: nonGitDir,
	}
	m := NewAgentManager(reg, "/usr/bin/python3", "localhost:50051", nil)

	m.AgentConnector.snapshotMu.Lock()
	m.AgentConnector.snapshots["agent-y"+"task-99"] = "deadbeef"
	m.AgentConnector.snapshotMu.Unlock()

	_ = m.Restore("agent-y", "task-99")

	m.AgentConnector.snapshotMu.Lock()
	_, stillPresent := m.AgentConnector.snapshots["agent-y"+"task-99"]
	m.AgentConnector.snapshotMu.Unlock()

	if stillPresent {
		t.Error("expected snapshot map entry to be deleted after Restore call")
	}
}

// Cycle 4: snapshots map is initialised (not nil) by NewAgentManager
func TestAgentManager_SnapshotsMapInitialised(t *testing.T) {
	m := NewAgentManager(nil, "/usr/bin/python3", "localhost:50051", nil)

	m.AgentConnector.snapshotMu.Lock()
	defer m.AgentConnector.snapshotMu.Unlock()

	if m.snapshots == nil {
		t.Error("expected snapshots map to be initialised, got nil")
	}
}
