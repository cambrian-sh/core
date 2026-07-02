package storage

import (
	"os"
	"path/filepath"
	"testing"
)

// ── Cycle 1: BBoltAdapter discovers "trait":"tool" from AGENT_MANIFEST ───────

// Test 1: A Python agent file with "trait":"tool" in AGENT_MANIFEST registers
// in BBolt with Trait = TraitTool.
func TestNewBBoltAdapter_TraitTool_RegistersWithTraitTool(t *testing.T) {
	agentsDir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "test.db")

	// Write a fake *agent.py with AGENT_MANIFEST containing "trait":"tool".
	agentFile := filepath.Join(agentsDir, "calculator_agent.py")
	content := `
AGENT_DESCRIPTION = "A deterministic calculator tool."

AGENT_MANIFEST = '''
{
    "version": "1.0.0",
    "trait": "tool",
    "supported_formats": ["json"]
}
'''
`
	if err := os.WriteFile(agentFile, []byte(content), 0600); err != nil {
		t.Fatalf("write agent file: %v", err)
	}

	adapter, err := NewBBoltAdapter(dbPath, agentsDir, nil)
	if err != nil {
		t.Fatalf("NewBBoltAdapter: %v", err)
	}
	defer adapter.Close()

	agents, err := adapter.GetAllAgentRecords()
	if err != nil {
		t.Fatalf("GetAllAgentRecords: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}

	got := agents[0]
	if got.Trait != "tool" {
		t.Errorf("expected Trait=%q, got %q", "tool", got.Trait)
	}
}

// Test 2: A Python agent file without a "trait" field registers with
// Trait = TraitCognitive (zero value, unchanged behaviour).
func TestNewBBoltAdapter_NoTrait_RegistersAsCognitive(t *testing.T) {
	agentsDir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "test.db")

	agentFile := filepath.Join(agentsDir, "writer_agent.py")
	content := `
AGENT_DESCRIPTION = "A cognitive writing agent."

AGENT_MANIFEST = '''
{
    "version": "1.0.0",
    "supported_formats": ["text"]
}
'''
`
	if err := os.WriteFile(agentFile, []byte(content), 0600); err != nil {
		t.Fatalf("write agent file: %v", err)
	}

	adapter, err := NewBBoltAdapter(dbPath, agentsDir, nil)
	if err != nil {
		t.Fatalf("NewBBoltAdapter: %v", err)
	}
	defer adapter.Close()

	agents, err := adapter.GetAllAgentRecords()
	if err != nil {
		t.Fatalf("GetAllAgentRecords: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}

	got := agents[0]
	if got.Trait != "" {
		t.Errorf("expected Trait=%q, got %q", "", got.Trait)
	}
}
