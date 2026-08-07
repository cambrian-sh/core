package storage

import (
	"path/filepath"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// Cycle 9 — Test 15:
// The real agents/system/ tree in this repo (retrieval_agent, reranker_agent,
// kg_extractor_agent — all package form after the migration) must be
// auto-discovered and registered with System=true. This is the integration
// check that ties the seeder walk + the kernel's domain.IsSystemAgent
// predicate together.
func TestSeed_RealSystemAgents_RegisteredAsSystem(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("abs repoRoot: %v", err)
	}
	agentsDir := filepath.Join(repoRoot, "agents")
	dbPath := filepath.Join(t.TempDir(), "test.db")

	adapter, err := NewBBoltAdapter(dbPath, agentsDir, domain.IsSystemAgent)
	if err != nil {
		t.Fatalf("NewBBoltAdapter: %v", err)
	}
	defer adapter.Close()

	agents, err := adapter.GetAllAgentRecords()
	if err != nil {
		t.Fatalf("GetAllAgentRecords: %v", err)
	}
	// The three agents this test is ABOUT must be present, system-flagged, and in
	// the package layout. It deliberately does NOT assert the total roster size:
	// the previous version required exactly 3 agents and the repo now ships 13, so
	// it failed on every added agent while telling you nothing about the property
	// it exists to check.
	wantIDs := map[string]bool{
		"reranker_agent":     false,
		"kg_extractor_agent": false,
		"retrieval_agent":    false,
	}
	for _, a := range agents {
		if _, ok := wantIDs[a.ID]; !ok {
			continue // another agent in the tree; not this test's business
		}
		wantIDs[a.ID] = true
		if !a.System {
			t.Errorf("%s.System: want true, got false", a.ID)
		}
		// ExecPath is RELATIVE to Dir — buildAgentCmd resolves it under
		// cmd.Dir=def.Dir, and an absolute path produces a doubled
		// "agents/agents/..." that python cannot open.
		wantExec := "system/" + a.ID + "/agent.py"
		if a.ExecPath != wantExec {
			t.Errorf("%s.ExecPath: want %q, got %q", a.ID, wantExec, a.ExecPath)
		}
	}
	for id, seen := range wantIDs {
		if !seen {
			t.Errorf("missing system agent: %q", id)
		}
	}
}
