package backfill

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// --- Test doubles ---

type mockAgentLister struct {
	agents []domain.AgentDefinition
	err    error
}

func (m *mockAgentLister) GetAllAgents(_ context.Context) ([]domain.AgentDefinition, error) {
	return m.agents, m.err
}

type mockProfileChecker struct {
	profile *domain.AgentProfile
	err     error
}

func (m *mockProfileChecker) GetProfile(_ context.Context, _, _ string) (*domain.AgentProfile, error) {
	return m.profile, m.err
}

type mockBackfillEnqueuer struct {
	enqueued []domain.AgentDefinition
}

func (m *mockBackfillEnqueuer) Enqueue(agent domain.AgentDefinition) {
	m.enqueued = append(m.enqueued, agent)
}

// alwaysAvailableEmbedder passes the health check immediately.
type alwaysAvailableEmbedder struct{}

func (e *alwaysAvailableEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return []float32{0.1}, nil
}

// --- Tests ---

// Cycle 1: provisional agents are enqueued; active (non-provisional) agents are skipped.
// A pre-existing profile in pgvector must not prevent a provisional agent from being
// interviewed — this is the mismatch that occurs when bbolt is wiped but pgvector
// persists. ADR-0023.
func TestRunInterviewBackfill_EnqueuesProvisionalAgents(t *testing.T) {
	agents := &mockAgentLister{
		agents: []domain.AgentDefinition{
			{ID: "provisional-no-profile", SourceHash: "h1", Provisional: true},
			{ID: "provisional-has-profile", SourceHash: "h2", Provisional: true},
			{ID: "active", SourceHash: "h3", Provisional: false},
		},
	}

	// Return a profile only for "provisional-has-profile".
	profileByID := map[string]*domain.AgentProfile{
		"provisional-has-profile": {AgentID: "provisional-has-profile", SourceHash: "h2"},
	}
	profiles := &perAgentProfileChecker{profiles: profileByID}

	enqueuer := &mockBackfillEnqueuer{}
	embedder := &alwaysAvailableEmbedder{}

	err := RunInterviewBackfill(context.Background(), agents, profiles, enqueuer, embedder, BackfillConfig{TimeoutMs: 5000})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(enqueuer.enqueued) != 2 {
		t.Fatalf("expected 2 enqueued agents, got %d: %v", len(enqueuer.enqueued), enqueuerIDs(enqueuer.enqueued))
	}
	ids := enqueuerIDs(enqueuer.enqueued)
	if ids[0] != "provisional-no-profile" || ids[1] != "provisional-has-profile" {
		t.Errorf("expected provisional agents to be enqueued, got %v", ids)
	}
}

// perAgentProfileChecker returns different profiles per agentID.
type perAgentProfileChecker struct {
	profiles map[string]*domain.AgentProfile
}

func (p *perAgentProfileChecker) GetProfile(_ context.Context, agentID, _ string) (*domain.AgentProfile, error) {
	return p.profiles[agentID], nil
}

func enqueuerIDs(agents []domain.AgentDefinition) []string {
	ids := make([]string, len(agents))
	for i, a := range agents {
		ids[i] = a.ID
	}
	return ids
}

// Cycle 2: Embedder unavailability triggers retry, not silent skip.
func TestRunInterviewBackfill_EmbedderUnavailableRetries(t *testing.T) {
	// Embedder fails twice, then succeeds on the third call.
	callCount := 0
	embedder := &countingEmbedder{fn: func() ([]float32, error) {
		callCount++
		if callCount < 3 {
			return nil, fmt.Errorf("embedder down")
		}
		return []float32{0.1}, nil
	}}

	agents := &mockAgentLister{
		agents: []domain.AgentDefinition{{ID: "agent-1", SourceHash: "h1", Provisional: true}},
	}
	profiles := &mockProfileChecker{profile: nil}
	enqueuer := &mockBackfillEnqueuer{}

	cfg := BackfillConfig{
		TimeoutMs:        5000, // 5 second window
		InitialBackoffMs: 1,    // 1 ms for fast tests
	}

	err := RunInterviewBackfill(context.Background(), agents, profiles, enqueuer, embedder, cfg)
	if err != nil {
		t.Fatalf("expected success after retry, got: %v", err)
	}

	// Must have retried (at least 3 embedder calls).
	if callCount < 3 {
		t.Errorf("expected at least 3 embedder calls (2 failures + 1 success), got %d", callCount)
	}

	// Agent must still be enqueued after embedder recovered.
	if len(enqueuer.enqueued) != 1 {
		t.Errorf("expected 1 enqueued agent after retry, got %d", len(enqueuer.enqueued))
	}
}

type countingEmbedder struct {
	fn func() ([]float32, error)
}

func (e *countingEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return e.fn()
}

// Cycle 3: RunInterviewBackfill returns quickly (server not blocked).
func TestRunInterviewBackfill_ReturnsAfterEnqueue_NotAfterProcessing(t *testing.T) {
	agents := &mockAgentLister{
		agents: []domain.AgentDefinition{
			{ID: "agent-1", SourceHash: "h1", Provisional: true},
			{ID: "agent-2", SourceHash: "h2", Provisional: true},
		},
	}
	profiles := &mockProfileChecker{profile: nil}

	// Non-blocking Enqueue (just records the call).
	enqueuer := &mockBackfillEnqueuer{}
	embedder := &alwaysAvailableEmbedder{}

	done := make(chan error, 1)
	go func() {
		done <- RunInterviewBackfill(context.Background(), agents, profiles, enqueuer, embedder, BackfillConfig{TimeoutMs: 5000})
	}()

	// Must complete within 500ms (enqueue is non-blocking, no processing waits).
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(enqueuer.enqueued) != 2 {
			t.Errorf("expected 2 enqueued agents, got %d", len(enqueuer.enqueued))
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("RunInterviewBackfill did not return within 500ms — it may be blocking on Interview processing")
	}
}
