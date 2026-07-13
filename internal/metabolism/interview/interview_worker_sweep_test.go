package interview

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// TestProcessAgent_TraitTool_TriggersSweep is the tracer bullet: after a
// successful TraitTool Provisional→Active transition, TriggerSweep is called.
func TestProcessAgent_TraitTool_TriggersSweep(t *testing.T) {
	agent := domain.AgentDefinition{
		ID:          "tool-sweep-agent",
		SourceHash:  "hash-ts-1",
		Trait:       domain.TraitTool,
		Description: "A deterministic tool",
		Provisional: true,
	}
	reg := newMockManifestReader(nil)
	embedder := &mockEmbedder{results: [][]float32{{0.1, 0.2}}}
	store := &mockProfileStore{}
	updater := &mockUpdater{}
	sweep := &mockSweepTrigger{}

	w := NewInterviewWorker(reg, embedder, store, updater)
	w.SweepTrigger = sweep

	if err := w.processAgent(context.Background(), agent); err != nil {
		t.Fatalf("processAgent returned error: %v", err)
	}

	if sweep.callCount != 1 {
		t.Errorf("expected TriggerSweep called once after TraitTool completion, got %d", sweep.callCount)
	}
}

// TestProcessAgent_TraitCognitive_TriggersSweep verifies TriggerSweep is called
// after the TraitCognitive Provisional→Active transition.
func TestProcessAgent_TraitCognitive_TriggersSweep(t *testing.T) {
	agent := domain.AgentDefinition{
		ID:          "cognitive-sweep-agent",
		SourceHash:  "hash-cs-1",
		Provisional: true,
	}
	reg := newMockManifestReader(nil)
	embedder := &mockEmbedder{results: [][]float32{{0.3, 0.4}}}
	store := &mockProfileStore{}
	updater := &mockUpdater{}
	sweep := &mockSweepTrigger{}

	w := NewInterviewWorker(reg, embedder, store, updater)
	w.SweepTrigger = sweep

	if err := w.processAgent(context.Background(), agent); err != nil {
		t.Fatalf("processAgent returned error: %v", err)
	}

	if sweep.callCount != 1 {
		t.Errorf("expected TriggerSweep called once after TraitCognitive completion, got %d", sweep.callCount)
	}
}

// TestProcessAgent_A2A_TriggersSweep verifies TriggerSweep is called after
// the A2A Provisional→Active transition.
func TestProcessAgent_A2A_TriggersSweep(t *testing.T) {
	agent := domain.AgentDefinition{
		ID:          "a2a-sweep-agent",
		SourceHash:  "hash-a2a-1",
		Runtime:     domain.RuntimeA2A,
		A2AEndpoint: "http://a2a.example.com",
		Provisional: true,
	}
	reg := newMockManifestReader(nil)
	card := &domain.AgentCard{Description: "A2A agent", Skills: []domain.A2ASkill{}}
	fetcher := &mockCardFetcher{card: card}
	embedder := &mockEmbedder{results: [][]float32{{0.5, 0.6}}}
	store := &mockProfileStore{}
	updater := &mockUpdater{}
	sweep := &mockSweepTrigger{}

	w := NewInterviewWorker(reg, embedder, store, updater)
	w.CardFetcher = fetcher
	w.SweepTrigger = sweep

	if err := w.processAgent(context.Background(), agent); err != nil {
		t.Fatalf("processAgent returned error: %v", err)
	}

	if sweep.callCount != 1 {
		t.Errorf("expected TriggerSweep called once after A2A completion, got %d", sweep.callCount)
	}
}

// TestProcessAgent_NilSweepTrigger_NoPanic verifies nil SweepTrigger does not
// panic on any path.
func TestProcessAgent_NilSweepTrigger_NoPanic(t *testing.T) {
	agent := domain.AgentDefinition{
		ID:          "no-sweep-agent",
		SourceHash:  "hash-ns-1",
		Provisional: true,
	}
	reg := newMockManifestReader(nil)
	embedder := &mockEmbedder{results: [][]float32{{0.1}}}
	store := &mockProfileStore{}
	updater := &mockUpdater{}

	w := NewInterviewWorker(reg, embedder, store, updater)
	// SweepTrigger deliberately left nil

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("nil SweepTrigger caused panic: %v", r)
		}
	}()

	if err := w.processAgent(context.Background(), agent); err != nil {
		t.Fatalf("processAgent returned error: %v", err)
	}
}
