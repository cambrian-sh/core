package domain

import "testing"

// ADR-0051: the Scout is a privileged system organ; ordinary agents are not.
func TestIsSystemAgent(t *testing.T) {
	t.Parallel()
	if !IsSystemAgent("retrieval_agent") {
		t.Error("retrieval_agent must be a system agent (bypasses interview, excluded from auction)")
	}
	// ADR-0053 D2 revised: the deterministic kg_extractor is also a privileged organ.
	if !IsSystemAgent("kg_extractor_agent") {
		t.Error("kg_extractor_agent must be a system agent (bypasses interview, excluded from auction)")
	}
	// The kernel-invoked organs that ran on DeterministicAgent ("tool" trait) now run on
	// CognitiveAgent, so this map — not the trait — keeps them out of the interview, the
	// Gatekeeper slate and the verifier pool (T-20261031-core-system-agents-docling-kg-extractor).
	for _, id := range []string{"reranker_agent", "docling_agent"} {
		if !IsSystemAgent(id) {
			t.Errorf("%q must be a system agent (kernel-invoked organ)", id)
		}
	}
	for _, id := range []string{"research_agent", "analyst_agent", "", "scout", "kg_extractor"} {
		if IsSystemAgent(id) {
			t.Errorf("%q must NOT be a system agent", id)
		}
	}
}
