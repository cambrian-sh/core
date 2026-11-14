package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"go.etcd.io/bbolt"
)

// ENG-1351 (owner decision 2026-11-14): a Python agent whose AGENT_MANIFEST
// declares the deleted "tool" trait is refused, not registered.
func TestNewBBoltAdapter_RetiredToolTrait_IsRefused(t *testing.T) {
	agentsDir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "test.db")
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
	if err := os.WriteFile(filepath.Join(agentsDir, "calculator_agent.py"), []byte(content), 0600); err != nil {
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
	if len(agents) != 0 {
		t.Fatalf("a retired-trait agent registered: %+v", agents)
	}
}

// ENG-1351: records stored with the retired "tool" trait before it was deleted
// are migrated at open: a system organ becomes cognitive (it stays excluded from
// selection and the verifier pool as a system definition), any other record is
// removed — so none becomes an ordinary cognitive verifier candidate.
func TestOpen_MigratesStoredToolTraitRecords(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := bbolt.Open(dbPath, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		if err := createBuckets(tx); err != nil {
			return err
		}
		bkt := tx.Bucket(agentBucket)
		for _, r := range []AgentRecord{
			{ID: "docling_agent", Trait: "tool", System: true},
			{ID: "third_party_calc", Trait: "tool"},
			{ID: "writer_agent", Trait: ""},
		} {
			b, _ := json.Marshal(r)
			if err := bkt.Put([]byte(r.ID), b); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	for i := 0; i < 2; i++ { // idempotent across opens
		adapter, err := NewBBoltAdapterNoScan(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		recs, err := adapter.GetAllAgentRecords()
		adapter.Close()
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]AgentRecord{}
		for _, r := range recs {
			got[r.ID] = r
		}
		if r, ok := got["docling_agent"]; !ok || r.Trait != "" || !r.System {
			t.Fatalf("open %d: system organ = %+v (present %v); want cognitive and still system", i, r, ok)
		}
		if _, ok := got["third_party_calc"]; ok {
			t.Fatalf("open %d: a non-system tool record survived", i)
		}
		if _, ok := got["writer_agent"]; !ok || len(got) != 2 {
			t.Fatalf("open %d: registry = %v", i, got)
		}
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
