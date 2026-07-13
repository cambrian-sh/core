package storage

import (
	"path/filepath"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// Cycle 9 — Test 15:
// The real agents/system/ tree in this repo (scout_agent, reranker_agent,
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
	if len(agents) != 3 {
		ids := make([]string, 0, len(agents))
		for _, a := range agents {
			ids = append(ids, a.ID)
		}
		t.Fatalf("expected 3 system agents, got %d (%v)", len(agents), ids)
	}

	wantIDs := map[string]bool{
		"scout_agent":       false,
		"reranker_agent":    false,
		"kg_extractor_agent": false,
	}
	for _, a := range agents {
		if _, ok := wantIDs[a.ID]; !ok {
			t.Errorf("unexpected agent: %q", a.ID)
			continue
		}
		wantIDs[a.ID] = true
		if !a.System {
			t.Errorf("%s.System: want true, got false", a.ID)
		}
		wantExec := filepath.ToSlash(filepath.Join(agentsDir, "system", a.ID, "agent.py"))
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
