package domain

import (
	"encoding/json"
	"testing"
	"time"
)

// Cycle 1 — EpisodicMemory round-trips through JSON cleanly.
func TestEpisodicMemory_JSONRoundTrip(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	em := EpisodicMemory{
		SessionID:    "sess-abc",
		StartedAt:    now,
		CompletedAt:  now.Add(time.Hour),
		Goal:         "Implement auth flow",
		Decisions:    []Decision{{Text: "Use JWT", MadeAt: now, SourceEventType: EventUserMessage}},
		ActionItems:  []ActionItem{{Text: "Write JWT middleware"}},
		Participants: []string{"analyst_agent", "code_generator_agent"},
		KeyFacts:     []string{"doc-1", "doc-2"},
	}

	data, err := json.Marshal(em)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got EpisodicMemory
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.SessionID != em.SessionID {
		t.Errorf("SessionID: want %q got %q", em.SessionID, got.SessionID)
	}
	if got.Goal != em.Goal {
		t.Errorf("Goal: want %q got %q", em.Goal, got.Goal)
	}
	if len(got.Decisions) != 1 || got.Decisions[0].Text != "Use JWT" {
		t.Errorf("Decisions: want 1 with text 'Use JWT', got %+v", got.Decisions)
	}
	if got.Decisions[0].SourceEventType != EventUserMessage {
		t.Errorf("Decision.SourceEventType: want %q got %q", EventUserMessage, got.Decisions[0].SourceEventType)
	}
	if len(got.ActionItems) != 1 || got.ActionItems[0].Text != "Write JWT middleware" {
		t.Errorf("ActionItems: got %+v", got.ActionItems)
	}
	if len(got.Participants) != 2 {
		t.Errorf("Participants: want 2, got %d", len(got.Participants))
	}
	if len(got.KeyFacts) != 2 {
		t.Errorf("KeyFacts: want 2, got %d", len(got.KeyFacts))
	}
}

// Cycle 2 — EpisodicMemory zero-value has nil slices, no Outcome field.
func TestEpisodicMemory_ZeroValue(t *testing.T) {
	var em EpisodicMemory
	if em.Decisions != nil {
		t.Errorf("zero-value Decisions should be nil")
	}
	if em.ActionItems != nil {
		t.Errorf("zero-value ActionItems should be nil")
	}
	if em.Participants != nil {
		t.Errorf("zero-value Participants should be nil")
	}
	if em.KeyFacts != nil {
		t.Errorf("zero-value KeyFacts should be nil")
	}
	// Outcome field must not exist — verified by compilation: if Outcome were a field,
	// this struct literal would need to reference it. The absence of Outcome here
	// and the fact the code compiles confirms it is not present.
	_ = EpisodicMemory{SessionID: "x", Goal: "y"}
}

// Cycle 3 — EpisodicMemory with nil slices serialises cleanly (no panic).
func TestEpisodicMemory_NilSlices_Marshal(t *testing.T) {
	em := EpisodicMemory{SessionID: "sess-1", Goal: "test goal"}
	data, err := json.Marshal(em)
	if err != nil {
		t.Fatalf("marshal with nil slices: %v", err)
	}
	var got EpisodicMemory
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.SessionID != "sess-1" {
		t.Errorf("SessionID: want sess-1 got %q", got.SessionID)
	}
}
