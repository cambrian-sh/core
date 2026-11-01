package verify

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// (b) ADR-0131, owner decision 2026-11-01: the originating session survives the
// queue as job data, and the verifier call runs under it with the
// background-spend marker — booked to the session, never refused at its
// ceiling. The worker's own ctx carries neither; only the verifier call does.
func TestVerificationWorker_VerifierCallCarriesOriginatingSessionAsBackgroundSpend(t *testing.T) {
	profiles := map[string]*domain.AgentProfile{"verifier1:vh1": {TrustScore: 0.9, SuccessRate: 0.9}}
	pool := &VerifierPool{
		Registry: newAgentSourceWith(
			domain.AgentDefinition{ID: "verifier1", SourceHash: "vh1"},
			domain.AgentDefinition{ID: "subject", SourceHash: "sh1"},
		),
		Profiles:      &vwMockGatekeeperReader{profiles: profiles},
		Threshold:     0.8,
		RecencyWindow: 3,
	}
	req := &vwMockVerifyRequester{called: make(chan struct{}, 10)}
	w := NewVerificationWorker(pool, req, &vwMockTaskEventRW{events: map[string]domain.TaskEvent{}},
		&vwMockJudicialStore{}, &vwMockProfileStore{profiles: map[string]*domain.AgentProfile{"subject:sh1": {TrustScore: 0.2}}}, &mockEmbedder{}, nil,
		VerificationWorkerConfig{QueueCapacity: 4, VerifierRecencyWindow: 3, TrustBoostThreshold: 0.4})

	// A Surveillance-mode subject (trust 0.2 < 0.4) is always sampled.
	enqCtx := domain.WithSessionID(context.Background(), "ses-v")
	h := &domain.Handoff{}
	if !w.Enqueue(enqCtx, findNonFNVTaskID(t), "subject", h, h) {
		t.Fatal("a Surveillance-mode subject must be sampled")
	}
	vreq := <-w.queue
	if vreq.SessionID != "ses-v" {
		t.Fatalf("job SessionID = %q, want the enqueuing step's session", vreq.SessionID)
	}

	w.processOne(context.Background(), vreq)
	req.mu.Lock()
	defer req.mu.Unlock()
	if req.callCount != 1 || req.lastSession != "ses-v" || !req.lastBackground {
		t.Fatalf("verifier call: count %d session %q background %v; want 1, ses-v, true",
			req.callCount, req.lastSession, req.lastBackground)
	}
}
