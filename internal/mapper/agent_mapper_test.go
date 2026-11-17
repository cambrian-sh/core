package mapper

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/storage"
)

// Cycle 0033-01: "daemon" trait string round-trips through DTO → mapper → domain.
func TestAgentMapper_TraitDaemon_RoundTrip(t *testing.T) {
	var m AgentMapper
	rec := storage.AgentRecord{ID: "gold-tracker", Trait: "daemon"}
	def := m.ToDomain(rec)
	if def.Trait != domain.TraitDaemon {
		t.Errorf("want TraitDaemon, got %q", def.Trait)
	}
	back := m.ToRecord(def)
	if back.Trait != "daemon" {
		t.Errorf("want \"daemon\" string in record, got %q", back.Trait)
	}
}

// TestAgentCapabilities_ZeroValue verifies that an AgentRecord without a capabilities
// key maps to a nil Capabilities slice on AgentDefinition — no error, no migration needed.
func TestAgentCapabilities_ZeroValue(t *testing.T) {
	var m AgentMapper
	rec := storage.AgentRecord{ID: "agent-1"}
	def := m.ToDomain(rec)
	if def.Capabilities != nil {
		t.Errorf("Capabilities: expected nil, got %v", def.Capabilities)
	}
}

// TestAgentCapabilities_RoundTrip verifies that a non-empty Capabilities slice survives
// the domain→record→domain round-trip without truncation or reordering.
func TestAgentCapabilities_RoundTrip(t *testing.T) {
	original := domain.AgentDefinition{
		ID:           "agent-1",
		Capabilities: []string{"vision", "text"},
	}
	var m AgentMapper
	rec := m.ToRecord(original)
	restored := m.ToDomain(rec)

	if len(restored.Capabilities) != len(original.Capabilities) {
		t.Fatalf("Capabilities length: got %d, want %d", len(restored.Capabilities), len(original.Capabilities))
	}
	for i, cap := range original.Capabilities {
		if restored.Capabilities[i] != cap {
			t.Errorf("Capabilities[%d]: got %q, want %q", i, restored.Capabilities[i], cap)
		}
	}
}

// TestSessionRoundTrip verifies that converting domain.Session → storage.SessionRecord
// → domain.Session produces an identical struct.
func TestSessionRoundTrip(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	original := domain.Session{
		ID:          "sess-001",
		ParentID:    "parent-001",
		Goal:        "Build HTTP bridge",
		Status:      domain.SessionActive,
		Summary:     "Work in progress",
		CreatedAt:   now,
		UpdatedAt:   now,
		CompletedAt: time.Time{},
	}

	var m AgentMapper
	rec := m.SessionToRecord(original)
	restored := m.SessionToDomain(rec)

	if restored.ID != original.ID {
		t.Errorf("ID: got %q, want %q", restored.ID, original.ID)
	}
	if restored.ParentID != original.ParentID {
		t.Errorf("ParentID: got %q, want %q", restored.ParentID, original.ParentID)
	}
	if restored.Goal != original.Goal {
		t.Errorf("Goal: got %q, want %q", restored.Goal, original.Goal)
	}
	if restored.Status != original.Status {
		t.Errorf("Status: got %q, want %q", restored.Status, original.Status)
	}
	if restored.Summary != original.Summary {
		t.Errorf("Summary: got %q, want %q", restored.Summary, original.Summary)
	}
	if !restored.CreatedAt.Equal(original.CreatedAt) {
		t.Errorf("CreatedAt: got %v, want %v", restored.CreatedAt, original.CreatedAt)
	}
	if !restored.UpdatedAt.Equal(original.UpdatedAt) {
		t.Errorf("UpdatedAt: got %v, want %v", restored.UpdatedAt, original.UpdatedAt)
	}
}

// TestSessionRoundTrip_AllStatuses verifies all SessionStatus values survive
// the domain→record→domain round-trip.
func TestSessionRoundTrip_AllStatuses(t *testing.T) {
	statuses := []domain.SessionStatus{
		domain.SessionActive,
		domain.SessionPaused,
		domain.SessionDormant,
		domain.SessionCompleted,
	}
	var m AgentMapper

	for _, status := range statuses {
		original := domain.Session{
			ID:     "sess-1",
			Status: status,
		}
		rec := m.SessionToRecord(original)
		restored := m.SessionToDomain(rec)
		if restored.Status != status {
			t.Errorf("Status %q: round-trip returned %q", status, restored.Status)
		}
	}
}

// TestSessionRoundTrip_CompletedAt verifies the CompletedAt timestamp survived.
func TestSessionRoundTrip_CompletedAt(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	original := domain.Session{
		ID:          "sess-1",
		Status:      domain.SessionCompleted,
		CompletedAt: now,
	}
	var m AgentMapper
	rec := m.SessionToRecord(original)
	restored := m.SessionToDomain(rec)
	if !restored.CompletedAt.Equal(original.CompletedAt) {
		t.Errorf("CompletedAt: got %v, want %v", restored.CompletedAt, original.CompletedAt)
	}
}

// TestSessionRoundTrip_CriticalData verifies the CriticalData field on the record
func TestSessionRoundTrip_EmptyCompletedAt(t *testing.T) {
	original := domain.Session{
		ID:          "sess-1",
		Status:      domain.SessionActive,
		CompletedAt: time.Time{},
	}
	var m AgentMapper
	rec := m.SessionToRecord(original)
	restored := m.SessionToDomain(rec)
	if !restored.CompletedAt.IsZero() {
		t.Errorf("CompletedAt: expected zero, got %v", restored.CompletedAt)
	}
	_ = rec
}

// TestTaskEventRoundTrip verifies that all ADR-0018/0019 token and cost fields survive
// the domain→record→domain round-trip (ADR-0021-01 data-loss fix).
func TestTaskEventRoundTrip(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	original := domain.TaskEvent{
		TaskID:               "task-001",
		AgentID:              "agent-42",
		SourceHash:           "hash-abc",
		BidConfidence:        0.85,
		VerifierScore:        0.92,
		NetworkLatencyMs:     45,
		ComputationLatencyMs: 120,
		ContextGrowthBytes:   1024,
		Timestamp:            now,
		Verified:             true,
		PromptTokens:         150,
		CompletionTokens:     80,
		TotalTokens:          230,
		EstimatedCost:        0.000345,
		BudgetOverrun:        true,
		FallbackModelUsed:    true,
		ActualModelID:        "qwen3:8b-fallback",
	}

	var m AgentMapper
	rec := m.TaskEventToRecord(original)
	restored := m.TaskEventToDomain(rec)

	if restored.TaskID != original.TaskID {
		t.Errorf("TaskID: got %q, want %q", restored.TaskID, original.TaskID)
	}
	if restored.PromptTokens != original.PromptTokens {
		t.Errorf("PromptTokens: got %d, want %d", restored.PromptTokens, original.PromptTokens)
	}
	if restored.CompletionTokens != original.CompletionTokens {
		t.Errorf("CompletionTokens: got %d, want %d", restored.CompletionTokens, original.CompletionTokens)
	}
	if restored.TotalTokens != original.TotalTokens {
		t.Errorf("TotalTokens: got %d, want %d", restored.TotalTokens, original.TotalTokens)
	}
	if restored.EstimatedCost != original.EstimatedCost {
		t.Errorf("EstimatedCost: got %f, want %f", restored.EstimatedCost, original.EstimatedCost)
	}
	if restored.BudgetOverrun != original.BudgetOverrun {
		t.Errorf("BudgetOverrun: got %v, want %v", restored.BudgetOverrun, original.BudgetOverrun)
	}
	if restored.FallbackModelUsed != original.FallbackModelUsed {
		t.Errorf("FallbackModelUsed: got %v, want %v", restored.FallbackModelUsed, original.FallbackModelUsed)
	}
	if restored.ActualModelID != original.ActualModelID {
		t.Errorf("ActualModelID: got %q, want %q", restored.ActualModelID, original.ActualModelID)
	}
}

// A stored stamp that does not parse still yields the zero time (a read must not
// fail over one bad stamp) but is no longer silent
// (T-20261113-persisted-writes-and-builder-resolver-errors).
func TestParseTime_MalformedStampIsLogged(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if got := parseTime(""); !got.IsZero() || buf.Len() != 0 {
		t.Fatalf("empty stamp: got %v, log %q; want zero time, no log", got, buf.String())
	}
	if got := parseTime("not-a-time"); !got.IsZero() {
		t.Fatalf("malformed stamp: got %v, want zero time", got)
	}
	if !strings.Contains(buf.String(), "does not parse") || !strings.Contains(buf.String(), "not-a-time") {
		t.Fatalf("malformed stamp must be logged with its value, log: %q", buf.String())
	}
	want := time.Date(2026, 11, 14, 1, 2, 3, 4, time.UTC)
	if got := parseTime(want.Format(time.RFC3339Nano)); !got.Equal(want) {
		t.Fatalf("round trip: got %v, want %v", got, want)
	}
}
