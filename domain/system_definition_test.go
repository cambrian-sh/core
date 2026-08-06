package domain

import "testing"

// System status has two routes, and only one of them can name a plugin's agent.
//
// The map below IsSystemAgent can only list organs compiled into this repo. The
// source discovery agent is kernel-invoked in exactly the same way — the ingress
// studio's discovery plane calls it directly and never routes it a task step —
// but it ships with a premium plugin, so naming it in OSS core would put an ID
// here for an agent that does not exist here.
func TestASystemGrantCountsAsMuchAsBeingNamedInTheMap(t *testing.T) {
	granted := AgentDefinition{ID: "source_discovery_agent", System: true}
	if !IsSystemDefinition(granted) {
		t.Error("an explicit AddSystemAgent grant must confer system status, or a plugin's kernel organ gets interviewed and auctioned")
	}
	// And the ID route still works, so nothing compiled-in regressed.
	if !IsSystemDefinition(AgentDefinition{ID: "scout_agent"}) {
		t.Error("scout_agent lost system status")
	}
	// The grant is not retroactive to the ID map: an agent nobody granted and
	// nobody named is ordinary, which is what keeps this from being a blanket.
	if IsSystemDefinition(AgentDefinition{ID: "source_discovery_agent"}) {
		t.Error("an ungranted plugin agent must NOT be privileged by its name alone")
	}
	if IsSystemDefinition(AgentDefinition{ID: "summariser_agent"}) {
		t.Error("an ordinary agent became privileged")
	}
}
