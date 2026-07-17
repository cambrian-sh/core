package storage

import (
	"path/filepath"
	"testing"
)

// UpsertDiscoveredAgent must be idempotent by SourceHash: re-upserting an agent whose
// SourceHash is unchanged leaves the existing record untouched — crucially preserving a
// post-interview Provisional=false state (ADR-0075). This is what lets the built-in
// filesystem agents be registered through an AgentSource without being re-interviewed on
// every boot.
func TestUpsertDiscoveredAgent_PreservesProvisionalOnUnchangedHash(t *testing.T) {
	db := filepath.Join(t.TempDir(), "t.db")
	store, err := NewBBoltAdapterNoScan(db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	da := DiscoveredAgent{Agent: AgentRecord{ID: "a1", SourceHash: "h1", Provisional: true}}
	if err := store.UpsertDiscoveredAgent(da); err != nil {
		t.Fatal(err)
	}
	// Simulate a completed interview: the agent is now non-provisional.
	if err := store.SetProvisional("a1", false); err != nil {
		t.Fatal(err)
	}

	// Re-upsert with the SAME SourceHash — must NOT re-provision.
	if err := store.UpsertDiscoveredAgent(da); err != nil {
		t.Fatal(err)
	}
	rec, err := store.GetAgentRecord("a1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Provisional {
		t.Fatal("unchanged-SourceHash re-upsert wrongly re-provisioned the agent (would re-interview every boot)")
	}

	// A CHANGED SourceHash DOES re-provision (source really changed).
	if err := store.UpsertDiscoveredAgent(DiscoveredAgent{Agent: AgentRecord{ID: "a1", SourceHash: "h2"}}); err != nil {
		t.Fatal(err)
	}
	rec, _ = store.GetAgentRecord("a1")
	if !rec.Provisional {
		t.Fatal("changed-SourceHash re-upsert should re-provision")
	}
}
