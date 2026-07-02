package verify

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/supervision/aggregator"
)

// ─── shouldSample pure function tests ────────────────────────────────────────

func TestShouldSample_SurveillanceMode_AlwaysTrue(t *testing.T) {
	nonFNVTaskID := findNonFNVTaskID(t)
	if !shouldSample(nonFNVTaskID, 0.39, 0.4) {
		t.Error("shouldSample must be true when TrustScore < threshold (Surveillance mode)")
	}
}

func TestShouldSample_AtThreshold_FNVOnly(t *testing.T) {
	nonFNVTaskID := findNonFNVTaskID(t)
	if shouldSample(nonFNVTaskID, 0.4, 0.4) {
		t.Error("shouldSample must be false when TrustScore == threshold and FNV does not fire")
	}
}

func TestShouldSample_Deterministic(t *testing.T) {
	taskID := "step-3-deadbeef99"
	first := shouldSample(taskID, 0.9, 0.4)
	for i := 0; i < 200; i++ {
		if shouldSample(taskID, 0.9, 0.4) != first {
			t.Fatal("shouldSample is not deterministic for the same taskID")
		}
	}
}

func TestShouldSample_ApproximatelyTenPercent(t *testing.T) {
	sampled := 0
	const total = 1000
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("step-%d-planxyz", i)
		if shouldSample(id, 0.9, 0.4) {
			sampled++
		}
	}
	rate := float64(sampled) / float64(total)
	if rate < 0.05 || rate > 0.15 {
		t.Errorf("sampling rate %.3f outside expected [0.05, 0.15]", rate)
	}
}

func TestConvergenceBound_DishonestAgentDecaysWithinBound(t *testing.T) {
	signals := make([]float64, convergenceBound)
	ts := aggregator.EWMA(signals, 0.5)
	if ts >= 0.1 {
		t.Errorf("TrustScore=%.6f after %d events; expected <0.1 (convergenceBound=%d violated)",
			ts, convergenceBound, convergenceBound)
	}
}

// ─── shouldCrossVerify pure function tests ───────────────────────────────────

func TestShouldCrossVerify_ReturnsFalse_AtZeroRate(t *testing.T) {
	for i := 0; i < 100; i++ {
		id := fmt.Sprintf("step-%d-cr", i)
		if shouldCrossVerify(id, "verifier1", 0) {
			t.Fatal("shouldCrossVerify must be false at rate=0")
		}
	}
}

func TestShouldCrossVerify_ReturnsTrue_AtFullRate(t *testing.T) {
	for i := 0; i < 100; i++ {
		id := fmt.Sprintf("step-%d-cr", i)
		if !shouldCrossVerify(id, "verifier1", 1.0) {
			t.Fatal("shouldCrossVerify must be true at rate=1.0")
		}
	}
}

func TestShouldCrossVerify_Deterministic(t *testing.T) {
	id := "step-42-abc"
	first := shouldCrossVerify(id, "verifier1", 0.05)
	for i := 0; i < 200; i++ {
		if shouldCrossVerify(id, "verifier1", 0.05) != first {
			t.Fatal("shouldCrossVerify is not deterministic for the same inputs")
		}
	}
}

func TestShouldCrossVerify_ApproximatesRate(t *testing.T) {
	const total = 10000
	const rate = 0.05
	count := 0
	for i := 0; i < total; i++ {
		if shouldCrossVerify(fmt.Sprintf("step-%d-cr", i), "v1", rate) {
			count++
		}
	}
	got := float64(count) / float64(total)
	if got < rate*0.4 || got > rate*2.5 {
		t.Errorf("cross-verify rate %.4f outside expected range for configured rate %.4f", got, rate)
	}
}

func TestVerificationWorker_CrossVerification_WritesVerifierTaskEvent(t *testing.T) {
	profiles := map[string]*domain.AgentProfile{
		"verifier1:vh1":     {TrustScore: 0.9, SuccessRate: 0.9},
		"crossverifier:vc1": {TrustScore: 0.85, SuccessRate: 0.85},
	}
	agents := []domain.AgentDefinition{
		{ID: "verifier1", SourceHash: "vh1", Provisional: false},
		{ID: "crossverifier", SourceHash: "vc1", Provisional: false},
	}
	taskID := "step-0-cv5test"
	events := &vwMockTaskEventRW{events: map[string]domain.TaskEvent{
		taskID: {TaskID: taskID, AgentID: "subject"},
	}}
	pool := &VerifierPool{
		Registry:      newAgentSourceWith(agents...),
		Profiles:      &vwMockGatekeeperReader{profiles: profiles},
		Threshold:     0.8,
		RecencyWindow: 3,
	}
	w := NewVerificationWorker(
		pool,
		&vwMockVerifyRequester{called: make(chan struct{}, 10)},
		events,
		&vwMockJudicialStore{},
		&vwMockProfileStore{profiles: map[string]*domain.AgentProfile{}},
		&mockEmbedder{},
		VerificationWorkerConfig{
			QueueCapacity: 256, VerifierRecencyWindow: 3,
			TrustBoostThreshold: 0.4, CrossVerifyRate: 1.0,
		},
	)
	w.processOne(context.Background(), domain.VerificationRequest{
		TaskID: taskID, AgentID: "subject", SourceHash: "ss1",
		BidConfidence: 0.8, Request: &domain.Handoff{}, Response: &domain.Handoff{Confidence: 0.8},
	})

	events.mu.Lock()
	defer events.mu.Unlock()
	crossTaskID := "crossverify-" + taskID
	cvEvent, ok := events.events[crossTaskID]
	if !ok {
		keys := make([]string, 0, len(events.events))
		for k := range events.events {
			keys = append(keys, k)
		}
		t.Fatalf("cross-verification TaskEvent %q not written; found: %v", crossTaskID, keys)
	}
	if cvEvent.AgentID == "subject" {
		t.Error("cross-verification TaskEvent must target primary verifier, not original subject")
	}
	if !cvEvent.Verified {
		t.Error("cross-verification TaskEvent.Verified must be true")
	}
}

func TestVerificationWorker_CrossVerification_IsTerminal(t *testing.T) {
	profiles := map[string]*domain.AgentProfile{
		"verifier1:vh1":     {TrustScore: 0.9, SuccessRate: 0.9},
		"crossverifier:vc1": {TrustScore: 0.85, SuccessRate: 0.85},
	}
	agents := []domain.AgentDefinition{
		{ID: "verifier1", SourceHash: "vh1", Provisional: false},
		{ID: "crossverifier", SourceHash: "vc1", Provisional: false},
	}
	taskID := "step-0-cv6test"
	events := &vwMockTaskEventRW{events: map[string]domain.TaskEvent{
		taskID: {TaskID: taskID, AgentID: "subject"},
	}}
	pool := &VerifierPool{
		Registry:      newAgentSourceWith(agents...),
		Profiles:      &vwMockGatekeeperReader{profiles: profiles},
		Threshold:     0.8,
		RecencyWindow: 3,
	}
	w := NewVerificationWorker(
		pool,
		&vwMockVerifyRequester{called: make(chan struct{}, 10)},
		events,
		&vwMockJudicialStore{},
		&vwMockProfileStore{profiles: map[string]*domain.AgentProfile{}},
		&mockEmbedder{},
		VerificationWorkerConfig{
			QueueCapacity: 256, VerifierRecencyWindow: 3,
			TrustBoostThreshold: 0.4, CrossVerifyRate: 1.0,
		},
	)
	w.processOne(context.Background(), domain.VerificationRequest{
		TaskID: taskID, AgentID: "subject", SourceHash: "ss1",
		BidConfidence: 0.8, Request: &domain.Handoff{}, Response: &domain.Handoff{Confidence: 0.8},
	})

	if n := len(w.queue); n != 0 {
		t.Errorf("queue has %d items after processOne; cross-verification must not enqueue further work", n)
	}
}

// ─── VerificationWorker.Enqueue tests ────────────────────────────────────────

func TestVerificationWorker_Enqueue_DropsWhenFull(t *testing.T) {
	w := newTestVerificationWorker(t, 1)

	reg := w.Pool.Registry.(*mockAgentSource)
	reg.agents["dropper"] = domain.AgentDefinition{ID: "dropper", SourceHash: "dh1"}
	ps := w.ProfileStore.(*vwMockProfileStore)
	ps.mu.Lock()
	ps.profiles["dropper:dh1"] = &domain.AgentProfile{TrustScore: 0.1}
	ps.mu.Unlock()

	h := &domain.Handoff{}
	id1 := findNonFNVTaskID(t) + "A"
	id2 := findNonFNVTaskID(t) + "B"

	w.Enqueue(t.Context(), id1, "dropper", h, h)
	result := w.Enqueue(t.Context(), id2, "dropper", h, h)
	if result {
		t.Error("Enqueue must return false when queue is full (drop)")
	}
	if w.DroppedCount() != 1 {
		t.Errorf("DroppedCount=%d, want 1", w.DroppedCount())
	}
}

func TestVerificationWorker_Enqueue_SurveillanceAlwaysEnqueues(t *testing.T) {
	w := newTestVerificationWorker(t, 256)

	reg := w.Pool.Registry.(*mockAgentSource)
	reg.agents["lowagent"] = domain.AgentDefinition{ID: "lowagent", SourceHash: "s1"}

	ps := w.ProfileStore.(*vwMockProfileStore)
	ps.mu.Lock()
	ps.profiles["lowagent:s1"] = &domain.AgentProfile{TrustScore: 0.2}
	ps.mu.Unlock()

	nonFNVID := findNonFNVTaskID(t)
	h := &domain.Handoff{}
	if !w.Enqueue(t.Context(), nonFNVID, "lowagent", h, h) {
		t.Error("Enqueue must return true for Surveillance-mode agent on non-FNV taskID")
	}
}

func TestVerificationWorker_Start_ProcessesItem(t *testing.T) {
	w := newTestVerificationWorker(t, 256)
	requester := w.Requester.(*vwMockVerifyRequester)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go w.Start(ctx)

	reg := w.Pool.Registry.(*mockAgentSource)
	reg.agents["verifier1"] = domain.AgentDefinition{
		ID: "verifier1", SourceHash: "vh1", Provisional: false,
	}
	ps := w.Pool.Profiles.(*vwMockGatekeeperReader)
	ps.mu.Lock()
	if ps.profiles == nil {
		ps.profiles = map[string]*domain.AgentProfile{}
	}
	ps.profiles["verifier1:vh1"] = &domain.AgentProfile{TrustScore: 0.9, SuccessRate: 0.9}
	ps.mu.Unlock()

	w.queue <- domain.VerificationRequest{
		TaskID: "step-0-direct", AgentID: "subjectagent", SourceHash: "sh1",
		BidConfidence: 0.8,
		Request:       &domain.Handoff{},
		Response:      &domain.Handoff{Confidence: 0.8},
	}

	select {
	case <-requester.called:
		// success
	case <-ctx.Done():
		t.Fatal("VerificationWorker did not call VerifyRequester within 2s")
	}
}

// ─── Issue #019: VerifyOutput RPC tests ──────────────────────────────────────

func TestVerificationWorker_VerifyOutput_CalledOnVerifier(t *testing.T) {
	profiles := map[string]*domain.AgentProfile{
		"verifier1:vh1": {TrustScore: 0.9, SuccessRate: 0.9},
	}
	agents := []domain.AgentDefinition{
		{ID: "verifier1", SourceHash: "vh1", Provisional: false},
	}
	taskID := "step-0-vr1test"
	events := &vwMockTaskEventRW{events: map[string]domain.TaskEvent{
		taskID: {TaskID: taskID, AgentID: "subject"},
	}}
	pool := &VerifierPool{
		Registry:      newAgentSourceWith(agents...),
		Profiles:      &vwMockGatekeeperReader{profiles: profiles},
		Threshold:     0.8,
		RecencyWindow: 3,
	}
	verifyReq := &vwMockVerifyRequester{called: make(chan struct{}, 10)}
	w := NewVerificationWorker(
		pool,
		verifyReq,
		events,
		&vwMockJudicialStore{},
		&vwMockProfileStore{profiles: map[string]*domain.AgentProfile{}},
		&mockEmbedder{},
		VerificationWorkerConfig{
			QueueCapacity: 256, VerifierRecencyWindow: 3,
			TrustBoostThreshold: 0.4, CrossVerifyRate: 0.0,
		},
	)
	w.processOne(context.Background(), domain.VerificationRequest{
		TaskID: taskID, AgentID: "subject", SourceHash: "ss1",
		BidConfidence: 0.8,
		Request:       &domain.Handoff{},
		Response:      &domain.Handoff{Confidence: 0.8},
	})

	verifyReq.mu.Lock()
	defer verifyReq.mu.Unlock()
	if verifyReq.callCount == 0 {
		t.Fatal("VerifyOutput must be called on the verifier; got 0 calls")
	}
	if verifyReq.lastReq == nil {
		t.Fatal("VerifyOutput was called with a nil VerifyRequest")
	}
	if verifyReq.lastReq.TaskID != taskID {
		t.Errorf("VerifyRequest.TaskID=%q, want %q", verifyReq.lastReq.TaskID, taskID)
	}
	if verifyReq.lastReq.WinnerAgentID != "subject" {
		t.Errorf("VerifyRequest.WinnerAgentID=%q, want \"subject\"", verifyReq.lastReq.WinnerAgentID)
	}
}

func TestVerificationWorker_VerifyOutput_ScoreWrittenToTaskEvent(t *testing.T) {
	profiles := map[string]*domain.AgentProfile{
		"verifier1:vh1": {TrustScore: 0.9, SuccessRate: 0.9},
	}
	agents := []domain.AgentDefinition{
		{ID: "verifier1", SourceHash: "vh1", Provisional: false},
	}
	taskID := "step-0-vr2test"
	events := &vwMockTaskEventRW{events: map[string]domain.TaskEvent{
		taskID: {TaskID: taskID, AgentID: "subject"},
	}}
	pool := &VerifierPool{
		Registry:      newAgentSourceWith(agents...),
		Profiles:      &vwMockGatekeeperReader{profiles: profiles},
		Threshold:     0.8,
		RecencyWindow: 3,
	}
	verifyReq := &vwMockVerifyRequester{
		called:      make(chan struct{}, 10),
		returnScore: 0.88,
		returnCrit:  "detailed critique",
	}
	w := NewVerificationWorker(
		pool,
		verifyReq,
		events,
		&vwMockJudicialStore{},
		&vwMockProfileStore{profiles: map[string]*domain.AgentProfile{}},
		&mockEmbedder{},
		VerificationWorkerConfig{
			QueueCapacity: 256, VerifierRecencyWindow: 3,
			TrustBoostThreshold: 0.4, CrossVerifyRate: 0.0,
		},
	)
	w.processOne(context.Background(), domain.VerificationRequest{
		TaskID: taskID, AgentID: "subject", SourceHash: "ss1",
		BidConfidence: 0.75,
		Request:       &domain.Handoff{},
		Response:      &domain.Handoff{Confidence: 0.75},
	})

	events.mu.Lock()
	defer events.mu.Unlock()
	ev, ok := events.events[taskID]
	if !ok {
		t.Fatal("TaskEvent not found after processOne")
	}
	if ev.VerifierScore != float64(float32(0.88)) {
		t.Errorf("TaskEvent.VerifierScore=%.4f, want %.4f", ev.VerifierScore, float64(float32(0.88)))
	}
	if !ev.Verified {
		t.Error("TaskEvent.Verified must be true")
	}
}

func TestVerificationWorker_VerifyOutput_CritiqueStoredAsJudicialRecord(t *testing.T) {
	profiles := map[string]*domain.AgentProfile{
		"verifier1:vh1": {TrustScore: 0.9, SuccessRate: 0.9},
	}
	agents := []domain.AgentDefinition{
		{ID: "verifier1", SourceHash: "vh1", Provisional: false},
	}
	taskID := "step-0-vr3test"
	events := &vwMockTaskEventRW{events: map[string]domain.TaskEvent{
		taskID: {TaskID: taskID, AgentID: "subject"},
	}}
	pool := &VerifierPool{
		Registry:      newAgentSourceWith(agents...),
		Profiles:      &vwMockGatekeeperReader{profiles: profiles},
		Threshold:     0.8,
		RecencyWindow: 3,
	}
	critique := "well structured output"
	verifyReq := &vwMockVerifyRequester{
		called:      make(chan struct{}, 10),
		returnScore: 0.9,
		returnCrit:  critique,
	}
	judicialStore := &vwMockJudicialStore{}
	w := NewVerificationWorker(
		pool,
		verifyReq,
		events,
		judicialStore,
		&vwMockProfileStore{profiles: map[string]*domain.AgentProfile{}},
		&mockEmbedder{},
		VerificationWorkerConfig{
			QueueCapacity: 256, VerifierRecencyWindow: 3,
			TrustBoostThreshold: 0.4, CrossVerifyRate: 0.0,
		},
	)
	w.processOne(context.Background(), domain.VerificationRequest{
		TaskID: taskID, AgentID: "subject", SourceHash: "ss1",
		BidConfidence: 0.8,
		Request:       &domain.Handoff{},
		Response:      &domain.Handoff{Confidence: 0.8},
	})

	judicialStore.mu.Lock()
	defer judicialStore.mu.Unlock()
	found := false
	for _, r := range judicialStore.records {
		if r == critique {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("JudicialStore.Save not called with critique %q; records=%v", critique, judicialStore.records)
	}
}

func TestVerificationWorker_VerifyOutput_CrossVerifyAlsoUsesVerifyOutput(t *testing.T) {
	profiles := map[string]*domain.AgentProfile{
		"verifier1:vh1":     {TrustScore: 0.9, SuccessRate: 0.9},
		"crossverifier:vc1": {TrustScore: 0.85, SuccessRate: 0.85},
	}
	agents := []domain.AgentDefinition{
		{ID: "verifier1", SourceHash: "vh1", Provisional: false},
		{ID: "crossverifier", SourceHash: "vc1", Provisional: false},
	}
	taskID := "step-0-vr4test"
	events := &vwMockTaskEventRW{events: map[string]domain.TaskEvent{
		taskID: {TaskID: taskID, AgentID: "subject"},
	}}
	pool := &VerifierPool{
		Registry:      newAgentSourceWith(agents...),
		Profiles:      &vwMockGatekeeperReader{profiles: profiles},
		Threshold:     0.8,
		RecencyWindow: 3,
	}
	verifyReq := &vwMockVerifyRequester{
		called:      make(chan struct{}, 10),
		returnScore: 0.9,
		returnCrit:  "good",
	}
	w := NewVerificationWorker(
		pool,
		verifyReq,
		events,
		&vwMockJudicialStore{},
		&vwMockProfileStore{profiles: map[string]*domain.AgentProfile{}},
		&mockEmbedder{},
		VerificationWorkerConfig{
			QueueCapacity: 256, VerifierRecencyWindow: 3,
			TrustBoostThreshold: 0.4, CrossVerifyRate: 1.0,
		},
	)
	w.processOne(context.Background(), domain.VerificationRequest{
		TaskID: taskID, AgentID: "subject", SourceHash: "ss1",
		BidConfidence: 0.8,
		Request:       &domain.Handoff{},
		Response:      &domain.Handoff{Confidence: 0.8},
	})

	verifyReq.mu.Lock()
	count := verifyReq.callCount
	verifyReq.mu.Unlock()
	if count < 2 {
		t.Errorf("VerifyOutput called %d times with CrossVerifyRate=1.0; want >= 2 (primary + cross)", count)
	}
}
