package storage

import "testing"

// A system grant is policy, and policy does not wait for a code change.
//
// System status was refreshed only on the "source changed" branch, so granting a
// plugin's agent system status did nothing until somebody happened to edit that
// agent's source. Observed live: the composition root logged the privileged
// grant at boot, and the kernel went on LLM-interviewing the agent anyway,
// because its stored record still said System=false from the previous boot.
func TestASystemGrantLandsWithoutTheAgentsSourceChanging(t *testing.T) {
	adapter, cleanup := newTestAdapter(t)
	defer cleanup()
	if err := adapter.db.Update(createBuckets); err != nil {
		t.Fatal(err)
	}

	da := DiscoveredAgent{
		Agent: AgentRecord{
			ID: "source_discovery_agent", Name: "source_discovery_agent",
			Description: "proposes how to read a source", Runtime: "python",
			ExecPath: "agent.py", Dir: "/plugins/x", SourceHash: "hash-unchanged",
			Trait: "cognitive", System: false,
		},
	}
	if err := adapter.UpsertDiscoveredAgent(da); err != nil {
		t.Fatal(err)
	}
	// The agent has since been interviewed, so it is no longer provisional. That
	// state must survive the grant — it is the reason the short-circuit exists.
	rec, err := adapter.GetAgentRecord("source_discovery_agent")
	if err != nil {
		t.Fatal(err)
	}
	rec.Provisional = false
	if err := adapter.UpsertDiscoveredAgent(DiscoveredAgent{Agent: *rec}); err != nil {
		t.Fatal(err)
	}

	// The plugin now grants system status. Same source, same hash.
	da.Agent.System = true
	if err := adapter.UpsertDiscoveredAgent(da); err != nil {
		t.Fatal(err)
	}

	got, err := adapter.GetAgentRecord("source_discovery_agent")
	if err != nil {
		t.Fatal(err)
	}
	if !got.System {
		t.Error("the grant did not land: the kernel will keep interviewing and auctioning an agent the composition root declared privileged")
	}
	if got.SourceHash != "hash-unchanged" {
		t.Errorf("the grant rewrote the source hash: %q", got.SourceHash)
	}
	if got.Provisional {
		t.Error("the grant marked the agent provisional — a routing-policy change must not force a re-interview")
	}

	// And it revokes, or a grant could never be taken back.
	da.Agent.System = false
	if err := adapter.UpsertDiscoveredAgent(da); err != nil {
		t.Fatal(err)
	}
	got, err = adapter.GetAgentRecord("source_discovery_agent")
	if err != nil {
		t.Fatal(err)
	}
	if got.System {
		t.Error("a revoked grant did not land")
	}
}
