package mapper

import (
	"testing"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/storage"
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

// TestSessionEventRoundTrip verifies SessionEvent domain→record→domain.
func TestSessionEventRoundTrip(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	original := domain.SessionEvent{
		SessionID:   "sess-001",
		Type:        domain.EventUserMessage,
		Timestamp:   now,
		Payload:     "Hello, build me an HTTP bridge",
		ArtifactIDs: []string{"hash-abc", "hash-def"},
	}

	var m AgentMapper
	rec := m.SessionEventToRecord(original)
	restored := m.SessionEventToDomain(rec)

	if restored.SessionID != original.SessionID {
		t.Errorf("SessionID: got %q, want %q", restored.SessionID, original.SessionID)
	}
	if restored.Type != original.Type {
		t.Errorf("Type: got %q, want %q", restored.Type, original.Type)
	}
	if !restored.Timestamp.Equal(original.Timestamp) {
		t.Errorf("Timestamp: got %v, want %v", restored.Timestamp, original.Timestamp)
	}
	if restored.Payload != original.Payload {
		t.Errorf("Payload: got %q, want %q", restored.Payload, original.Payload)
	}
	if len(restored.ArtifactIDs) != len(original.ArtifactIDs) {
		t.Fatalf("ArtifactIDs length: got %d, want %d", len(restored.ArtifactIDs), len(original.ArtifactIDs))
	}
	for i, id := range original.ArtifactIDs {
		if restored.ArtifactIDs[i] != id {
			t.Errorf("ArtifactIDs[%d]: got %q, want %q", i, restored.ArtifactIDs[i], id)
		}
	}
}

// TestSessionEventRoundTrip_AllTypes verifies all event type constants survive the round-trip.
func TestSessionEventRoundTrip_AllTypes(t *testing.T) {
	types := []domain.SessionEventType{
		domain.EventUserMessage,
		domain.EventSystemThought,
		domain.EventHITLIntervention,
		domain.EventCriticalError,
		domain.EventBudgetBreach,
		domain.EventCheckpointSaved,
	}
	var m AgentMapper

	for _, evType := range types {
		original := domain.SessionEvent{SessionID: "s1", Type: evType}
		rec := m.SessionEventToRecord(original)
		restored := m.SessionEventToDomain(rec)
		if restored.Type != evType {
			t.Errorf("Type %q: round-trip returned %q", evType, restored.Type)
		}
	}
}

// TestSessionEventRoundTrip_NilArtifactIDs verifies nil ArtifactIDs produce empty slice.
func TestSessionEventRoundTrip_NilArtifactIDs(t *testing.T) {
	original := domain.SessionEvent{
		SessionID:   "s1",
		Type:        domain.EventSystemThought,
		ArtifactIDs: nil,
	}
	var m AgentMapper
	rec := m.SessionEventToRecord(original)
	if len(rec.ArtifactIDs) != 0 {
		t.Errorf("record ArtifactIDs: expected empty, got %v", rec.ArtifactIDs)
	}
	restored := m.SessionEventToDomain(rec)
	if len(restored.ArtifactIDs) != 0 {
		t.Errorf("restored ArtifactIDs: expected empty, got %v", restored.ArtifactIDs)
	}
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
		ComputationLatencyMs:  120,
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

// TestArtifactRoundTrip verifies Artifact domain→record→domain.
func TestArtifactRoundTrip(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	original := domain.Artifact{
		Hash:        "abc123def456",
		ContentType: "text/csv",
		SizeBytes:   2048,
		SessionID:   "sess-001",
		StepIndex:   2,
		Metadata:    map[string]string{"name": "User Report", "purpose": "Generated user activity report"},
		ParentHash:  "parent-hash",
		CreatedAt:   now,
	}

	var m AgentMapper
	rec := m.ArtifactToRecord(original)
	restored := m.ArtifactToDomain(rec)

	if restored.Hash != original.Hash {
		t.Errorf("Hash: got %q, want %q", restored.Hash, original.Hash)
	}
	if restored.ContentType != original.ContentType {
		t.Errorf("ContentType: got %q, want %q", restored.ContentType, original.ContentType)
	}
	if restored.SizeBytes != original.SizeBytes {
		t.Errorf("SizeBytes: got %d, want %d", restored.SizeBytes, original.SizeBytes)
	}
	if restored.SessionID != original.SessionID {
		t.Errorf("SessionID: got %q, want %q", restored.SessionID, original.SessionID)
	}
	if restored.StepIndex != original.StepIndex {
		t.Errorf("StepIndex: got %d, want %d", restored.StepIndex, original.StepIndex)
	}
	if restored.ParentHash != original.ParentHash {
		t.Errorf("ParentHash: got %q, want %q", restored.ParentHash, original.ParentHash)
	}
	if !restored.CreatedAt.Equal(original.CreatedAt) {
		t.Errorf("CreatedAt: got %v, want %v", restored.CreatedAt, original.CreatedAt)
	}
	if restored.Metadata["name"] != original.Metadata["name"] {
		t.Errorf("Metadata[name]: got %q, want %q", restored.Metadata["name"], original.Metadata["name"])
	}
}

// TestArtifactRoundTrip_NilMetadata verifies nil Metadata produces empty map.
func TestArtifactRoundTrip_NilMetadata(t *testing.T) {
	original := domain.Artifact{
		Hash:      "hash-1",
		Metadata:  nil,
	}
	var m AgentMapper
	rec := m.ArtifactToRecord(original)
	restored := m.ArtifactToDomain(rec)
	if len(restored.Metadata) != 0 {
		t.Errorf("Metadata: expected empty, got %v", restored.Metadata)
	}
	_ = rec
}
