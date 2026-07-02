package domain

import "testing"

// ADR-0051: the Scout is a privileged system organ; ordinary agents are not.
func TestIsSystemAgent(t *testing.T) {
	if !IsSystemAgent("scout_agent") {
		t.Error("scout_agent must be a system agent (bypasses interview, excluded from auction)")
	}
	// ADR-0053 D2 revised: the deterministic kg_extractor is also a privileged organ.
	if !IsSystemAgent("kg_extractor_agent") {
		t.Error("kg_extractor_agent must be a system agent (bypasses interview, excluded from auction)")
	}
	for _, id := range []string{"research_agent", "analyst_agent", "", "scout", "kg_extractor"} {
		if IsSystemAgent(id) {
			t.Errorf("%q must NOT be a system agent", id)
		}
	}
}
