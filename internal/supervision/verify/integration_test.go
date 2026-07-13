package verify

import (
	"context"
	"fmt"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

func TestIntegration_VerifierPoolEmptyDegradation(t *testing.T) {
	agents := []domain.AgentDefinition{
		{ID: "low-v1", SourceHash: "lv1", Provisional: false},
		{ID: "low-v2", SourceHash: "lv2", Provisional: false},
	}
	profiles := map[string]*domain.AgentProfile{
		"low-v1:lv1": {TrustScore: 0.3, SuccessRate: 0.3},
		"low-v2:lv2": {TrustScore: 0.3, SuccessRate: 0.3},
	}
	pool := &VerifierPool{
		Registry:      newAgentSourceWith(agents...),
		Profiles:      &vwMockGatekeeperReader{profiles: profiles},
		Threshold:     0.8,
		RecencyWindow: 3,
	}

	task := &domain.AuctionTask{ID: "t-s4"}
	_, err := pool.Select(context.Background(), task, "winner", nil)
	if err != ErrNoVerifierAvailable {
		t.Errorf("expected ErrNoVerifierAvailable from empty pool, got %v", err)
	}

	const taskID = "task-s4"
	events := &vwMockTaskEventRW{
		events: map[string]domain.TaskEvent{
			taskID: {TaskID: taskID, AgentID: "winner"},
		},
	}
	w := NewVerificationWorker(
		pool,
		&vwMockVerifyRequester{called: make(chan struct{}, 1)},
		events,
		&vwMockJudicialStore{},
		&vwMockProfileStore{profiles: map[string]*domain.AgentProfile{
			"winner:ws1": {TrustScore: 0.2},
		}},
		&mockEmbedder{},
		VerificationWorkerConfig{
			QueueCapacity: 256, VerifierRecencyWindow: 3, TrustBoostThreshold: 0.4,
		},
	)

	vreq := domain.VerificationRequest{
		TaskID: taskID, AgentID: "winner", SourceHash: "ws1",
		BidConfidence: 0.8,
		Request:       &domain.Handoff{},
		Response:      &domain.Handoff{},
	}

	w.processOne(context.Background(), vreq)

	events.mu.Lock()
	defer events.mu.Unlock()
	original, ok := events.events[taskID]
	if !ok {
		t.Fatal("original TaskEvent was unexpectedly removed")
	}
	if original.Verified {
		t.Error("TaskEvent.Verified must remain false when the Verifier Pool is empty")
	}
}

func TestIntegration_SamplingDeterminism(t *testing.T) {
	const total = 100
	const boostThreshold = 0.4

	ids := make([]string, total)
	for i := range ids {
		ids[i] = fmt.Sprintf("step-%d-intg-det", i)
	}

	const fnvTrustScore = 0.6
	decisions1 := make([]bool, total)
	decisions2 := make([]bool, total)

	for i, id := range ids {
		decisions1[i] = shouldSample(id, fnvTrustScore, boostThreshold)
	}
	for i, id := range ids {
		decisions2[i] = shouldSample(id, fnvTrustScore, boostThreshold)
	}
	for i := range ids {
		if decisions1[i] != decisions2[i] {
			t.Errorf("FNV path: task %q decisions differ (%v vs %v)", ids[i], decisions1[i], decisions2[i])
		}
	}

	const survTrustScore = 0.2
	for _, id := range ids {
		d1 := shouldSample(id, survTrustScore, boostThreshold)
		d2 := shouldSample(id, survTrustScore, boostThreshold)
		if !d1 || !d2 {
			t.Errorf("Surveillance path: task %q must always sample (got %v, %v)", id, d1, d2)
		}
		if d1 != d2 {
			t.Errorf("Surveillance path: task %q decisions differ (%v vs %v)", id, d1, d2)
		}
	}
}
