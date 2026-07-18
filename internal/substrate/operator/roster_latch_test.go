package operator

import (
	"context"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// countAgentReady reads the whole retained spool and counts AgentReadyEvents per agent.
func countAgentReady(feed *Spool) map[string]int {
	evs, _ := feed.ReadFrom(0)
	out := map[string]int{}
	for _, se := range evs {
		if ev, ok := se.Event.(domain.AgentReadyEvent); ok {
			out[ev.AgentID]++
		}
	}
	return out
}

func TestRosterLatch_ReemitsCachedRoster(t *testing.T) {
	feed := NewSpool(SpoolConfig{})
	latch := NewRosterLatch(feed)

	// Two agents become ready (as the bridge would have forwarded them).
	latch.Observe(domain.AgentReadyEvent{AgentID: "a", Capabilities: []string{"code_search"}})
	latch.Observe(domain.AgentReadyEvent{AgentID: "b", Capabilities: []string{"file_read"}})
	// A later readiness for 'a' with updated caps — last write wins.
	latch.Observe(domain.AgentReadyEvent{AgentID: "a", Capabilities: []string{"code_search", "file_read"}})

	// A heartbeat re-emits the current roster to the feed.
	latch.reemit()

	got := countAgentReady(feed)
	if got["a"] != 1 || got["b"] != 1 {
		t.Fatalf("expected one re-emitted ready per agent, got %v", got)
	}
	// Verify 'a' carries the LATEST capabilities.
	evs, _ := feed.ReadFrom(0)
	for _, se := range evs {
		if ev, ok := se.Event.(domain.AgentReadyEvent); ok && ev.AgentID == "a" {
			if len(ev.Capabilities) != 2 {
				t.Errorf("latch did not keep the latest caps for 'a': %v", ev.Capabilities)
			}
		}
	}
}

// fakeCapSource is a minimal AgentCapabilitySource for Seed.
type fakeCapSource struct {
	agents    []domain.AgentDefinition
	manifests map[string]*domain.AgentManifest
}

func (f *fakeCapSource) GetAllAgents(context.Context) ([]domain.AgentDefinition, error) {
	return f.agents, nil
}
func (f *fakeCapSource) GetManifest(_ context.Context, id string) (*domain.AgentManifest, error) {
	return f.manifests[id], nil
}

// Seeding from manifests must populate the roster even with NO AgentReadyEvents
// (the disable_interviews case) — this is the actual empty-capabilities root cause.
func TestRosterLatch_SeedFromManifests(t *testing.T) {
	feed := NewSpool(SpoolConfig{})
	latch := NewRosterLatch(feed)
	src := &fakeCapSource{
		agents: []domain.AgentDefinition{{ID: "code_generator_agent"}, {ID: "toolsonly_agent"}, {ID: "bare_agent"}},
		manifests: map[string]*domain.AgentManifest{
			"code_generator_agent": {Capabilities: []string{"code_search", "file_read"}},
			"toolsonly_agent":      {Tools: []string{"fs_read"}}, // falls back to tools
			"bare_agent":           {},                            // no caps/tools ⇒ skipped
		},
	}
	latch.Seed(context.Background(), src)
	latch.reemit()

	got := countAgentReady(feed)
	if got["code_generator_agent"] != 1 || got["toolsonly_agent"] != 1 {
		t.Fatalf("seed did not populate roster from manifests: %v", got)
	}
	if _, ok := got["bare_agent"]; ok {
		t.Errorf("agent with no capabilities/tools should be skipped")
	}
}

func TestRosterLatch_ObserveIgnoresNonReady(t *testing.T) {
	latch := NewRosterLatch(NewSpool(SpoolConfig{}))
	latch.Observe(domain.ScoutUsefulnessEvent{}) // wrong type — ignored
	latch.Observe(domain.AgentReadyEvent{AgentID: ""}) // empty id — ignored
	if len(latch.roster) != 0 {
		t.Errorf("latch cached an invalid event: %v", latch.roster)
	}
}

func TestRosterLatch_StartHeartbeat(t *testing.T) {
	feed := NewSpool(SpoolConfig{})
	latch := NewRosterLatch(feed)
	latch.Observe(domain.AgentReadyEvent{AgentID: "a", Capabilities: []string{"x"}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	latch.Start(ctx, 20*time.Millisecond)

	// Wait for at least one heartbeat re-emit.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if countAgentReady(feed)["a"] >= 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("heartbeat did not re-emit the roster within the deadline")
}
