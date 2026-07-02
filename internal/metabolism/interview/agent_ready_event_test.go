package interview

import (
	"context"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// captureEventBus records domain events published to it.
type captureEventBus struct {
	events []domain.DomainEvent
}

func (b *captureEventBus) Subscribe(_ string, _ domain.EventHandler) {}
func (b *captureEventBus) Publish(e domain.DomainEvent) error {
	b.events = append(b.events, e)
	return nil
}

// TestAgentReadyEvent_EmittedAfterToolInterview verifies that processAgent
// publishes an AgentReadyEvent after a TraitTool agent becomes Active.
func TestAgentReadyEvent_EmittedAfterToolInterview(t *testing.T) {
	agent := domain.AgentDefinition{
		ID:          "tool-agent",
		SourceHash:  "hash-tool",
		Trait:       domain.TraitTool,
		Provisional: true,
	}
	registry := newTestRegistry(agent)
	embedder := &mockEmbedder{results: [][]float32{{0.1, 0.2}}}
	store := &mockProfileStore{}
	updater := &mockUpdater{}
	bus := &captureEventBus{}

	w := newWorker(registry, embedder, store, updater)
	w.EventBus = bus

	if err := w.processAgent(context.Background(), agent); err != nil {
		t.Fatalf("processAgent returned error: %v", err)
	}

	var found *domain.AgentReadyEvent
	for _, e := range bus.events {
		if ev, ok := e.(domain.AgentReadyEvent); ok {
			found = &ev
			break
		}
	}
	if found == nil {
		t.Fatal("expected AgentReadyEvent to be published, got none")
	}
	if found.AgentID != agent.ID {
		t.Errorf("expected AgentID %q, got %q", agent.ID, found.AgentID)
	}
}

// TestAgentReadyEvent_EmittedAfterCognitiveInterview verifies emission for
// TraitCognitive agents too (the non-tool path through processAgent).
func TestAgentReadyEvent_EmittedAfterCognitiveInterview(t *testing.T) {
	agent := domain.AgentDefinition{
		ID:          "cog-agent",
		SourceHash:  "hash-cog",
		Trait:       domain.TraitCognitive,
		Provisional: true,
	}
	registry := newTestRegistry(agent)
	embedder := &mockEmbedder{results: [][]float32{{0.1, 0.2}}}
	store := &mockProfileStore{}
	updater := &mockUpdater{}
	bus := &captureEventBus{}

	w := newWorker(registry, embedder, store, updater)
	w.EventBus = bus

	if err := w.processAgent(context.Background(), agent); err != nil {
		t.Fatalf("processAgent returned error: %v", err)
	}

	var readyCount int
	for _, e := range bus.events {
		if ev, ok := e.(domain.AgentReadyEvent); ok {
			readyCount++
			if ev.AgentID != agent.ID {
				t.Errorf("expected AgentID %q, got %q", agent.ID, ev.AgentID)
			}
		}
	}
	if readyCount != 1 {
		t.Errorf("expected exactly 1 AgentReadyEvent, got %d", readyCount)
	}
}

// TestAgentReadyEvent_NilBusIsSafe verifies no panic when EventBus is nil.
func TestAgentReadyEvent_NilBusIsSafe(t *testing.T) {
	agent := domain.AgentDefinition{
		ID: "agent-no-bus", SourceHash: "hash", Trait: domain.TraitTool, Provisional: true,
	}
	w := newWorker(newTestRegistry(agent), &mockEmbedder{results: [][]float32{{0.1}}},
		&mockProfileStore{}, &mockUpdater{})
	w.EventBus = nil // explicit nil

	if err := w.processAgent(context.Background(), agent); err != nil {
		t.Fatalf("processAgent with nil EventBus returned error: %v", err)
	}
}
