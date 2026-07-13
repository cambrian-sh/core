package aggregator

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ─── Pure function tests ──────────────────────────────────────────────────────

func TestEWMA_SingleValue(t *testing.T) {
	got := EWMA([]float64{1.0}, 0.3)
	if got != 1.0 {
		t.Errorf("EWMA([1.0], 0.3) = %.6f, want 1.0", got)
	}
}

func TestEWMA_TwoValues(t *testing.T) {
	got := EWMA([]float64{1.0, 0.0}, 0.3)
	if got <= 0 || got >= 1.0 {
		t.Errorf("EWMA([1.0, 0.0], 0.3) = %.6f, want (0, 1.0)", got)
	}
}

func TestEWMA_Nil(t *testing.T) {
	got := EWMA(nil, 0.3)
	if got != 0.0 {
		t.Errorf("EWMA(nil, 0.3) = %.6f, want 0.0", got)
	}
	got2 := EWMA([]float64{}, 0.3)
	if got2 != 0.0 {
		t.Errorf("EWMA([], 0.3) = %.6f, want 0.0", got2)
	}
}

func TestRollingMedian_SmallSlice(t *testing.T) {
	got := RollingMedian([]int{10, 20, 30}, 10)
	if got != 20.0 {
		t.Errorf("RollingMedian([10,20,30], 10) = %.1f, want 20.0", got)
	}
}

func TestRollingMedian_Window(t *testing.T) {
	got := RollingMedian([]int{10, 20, 30, 40, 50}, 3)
	if got != 40.0 {
		t.Errorf("RollingMedian([10,20,30,40,50], 3) = %.1f, want 40.0", got)
	}
}

func TestRollingMedian_Nil(t *testing.T) {
	got := RollingMedian(nil, 10)
	if got != 0.0 {
		t.Errorf("RollingMedian(nil, 10) = %.1f, want 0.0", got)
	}
}

// ─── Mock helpers ─────────────────────────────────────────────────────────────

type mockTaskEventReader struct {
	events    map[string][]domain.TaskEvent
	agentKeys []string
}

func (r *mockTaskEventReader) ReadTaskEvents(agentID, sourceHash string) ([]domain.TaskEvent, error) {
	key := agentID + ":" + sourceHash
	return r.events[key], nil
}

func (r *mockTaskEventReader) ReadAllAgentIDs() ([]string, error) {
	return r.agentKeys, nil
}

type mockAggregatorStore struct {
	savedProfiles   []aggregatorSavedCall
	profileToReturn *domain.AgentProfile
}

type aggregatorSavedCall struct {
	AgentID    string
	SourceHash string
	Embedding  []float32
	Profile    domain.AgentProfile
}

func (s *mockAggregatorStore) SaveProfile(_ context.Context, agentID, sourceHash string, embedding []float32, profile domain.AgentProfile) error {
	s.savedProfiles = append(s.savedProfiles, aggregatorSavedCall{
		AgentID:    agentID,
		SourceHash: sourceHash,
		Embedding:  embedding,
		Profile:    profile,
	})
	return nil
}

func (s *mockAggregatorStore) GetProfile(_ context.Context, _, _ string) (*domain.AgentProfile, error) {
	return s.profileToReturn, nil
}

func abs64(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// ─── ProfileAggregator.RunOnce tests ─────────────────────────────────────────

func TestRunOnce_VerifiedEventsSuccessRate(t *testing.T) {
	events := []domain.TaskEvent{
		{AgentID: "a1", SourceHash: "h1", Verified: true, VerifierScore: 1.0, BidConfidence: 0.8},
		{AgentID: "a1", SourceHash: "h1", Verified: true, VerifierScore: 1.0, BidConfidence: 0.8},
		{AgentID: "a1", SourceHash: "h1", Verified: true, VerifierScore: 0.0, BidConfidence: 0.8},
	}
	reader := &mockTaskEventReader{
		events:    map[string][]domain.TaskEvent{"a1:h1": events},
		agentKeys: []string{"a1:h1"},
	}
	store := &mockAggregatorStore{}
	cfg := AggregatorConfig{IntervalSeconds: 60, EWMAAlpha: 0.3, LatencyWindow: 50}
	agg := New(reader, store, cfg)

	if err := agg.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce error: %v", err)
	}
	if len(store.savedProfiles) != 1 {
		t.Fatalf("expected 1 saved profile, got %d", len(store.savedProfiles))
	}
	sr := store.savedProfiles[0].Profile.SuccessRate
	if sr <= 0 || sr >= 1 {
		t.Errorf("SuccessRate = %.4f, want (0, 1)", sr)
	}
}

func TestRunOnce_NoVerifiedEvents_NeutralTrustScore(t *testing.T) {
	events := []domain.TaskEvent{
		{AgentID: "a2", SourceHash: "h2", Verified: false, BidConfidence: 0.7},
		{AgentID: "a2", SourceHash: "h2", Verified: false, BidConfidence: 0.7},
	}
	reader := &mockTaskEventReader{
		events:    map[string][]domain.TaskEvent{"a2:h2": events},
		agentKeys: []string{"a2:h2"},
	}
	store := &mockAggregatorStore{}
	cfg := AggregatorConfig{IntervalSeconds: 60, EWMAAlpha: 0.3, LatencyWindow: 50}
	agg := New(reader, store, cfg)

	if err := agg.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce error: %v", err)
	}
	if len(store.savedProfiles) != 1 {
		t.Fatalf("expected 1 saved profile, got %d", len(store.savedProfiles))
	}
	ts := store.savedProfiles[0].Profile.TrustScore
	if ts != 0.5 {
		t.Errorf("TrustScore = %.4f, want 0.5 (neutral)", ts)
	}
}

func TestRunOnce_VerifiedEvents_TrustScoreTrendsToHalf(t *testing.T) {
	events := []domain.TaskEvent{
		{AgentID: "a3", SourceHash: "h3", Verified: true, VerifierScore: 0.5, BidConfidence: 0.5},
		{AgentID: "a3", SourceHash: "h3", Verified: true, VerifierScore: 0.5, BidConfidence: 0.5},
		{AgentID: "a3", SourceHash: "h3", Verified: true, VerifierScore: 0.5, BidConfidence: 0.5},
	}
	reader := &mockTaskEventReader{
		events:    map[string][]domain.TaskEvent{"a3:h3": events},
		agentKeys: []string{"a3:h3"},
	}
	store := &mockAggregatorStore{}
	cfg := AggregatorConfig{IntervalSeconds: 60, EWMAAlpha: 0.3, LatencyWindow: 50}
	agg := New(reader, store, cfg)

	if err := agg.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce error: %v", err)
	}
	if len(store.savedProfiles) != 1 {
		t.Fatalf("expected 1 saved profile, got %d", len(store.savedProfiles))
	}
	ts := store.savedProfiles[0].Profile.TrustScore
	if abs64(ts-0.5) > 1e-9 {
		t.Errorf("TrustScore = %.6f, want ~0.5", ts)
	}
}

func TestRunOnce_SaveProfileCalledOncePerAgent(t *testing.T) {
	reader := &mockTaskEventReader{
		events: map[string][]domain.TaskEvent{
			"a4:h4": {{AgentID: "a4", SourceHash: "h4", Verified: true, VerifierScore: 0.8, BidConfidence: 0.7}},
			"a5:h5": {{AgentID: "a5", SourceHash: "h5", Verified: false}},
		},
		agentKeys: []string{"a4:h4", "a5:h5"},
	}
	store := &mockAggregatorStore{}
	cfg := AggregatorConfig{IntervalSeconds: 60, EWMAAlpha: 0.3, LatencyWindow: 50}
	agg := New(reader, store, cfg)

	if err := agg.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce error: %v", err)
	}
	if len(store.savedProfiles) != 2 {
		t.Errorf("expected SaveProfile called 2 times (once per agent), got %d", len(store.savedProfiles))
	}
}

// ─── D1 / D3 / D5 tests ──────────────────────────────────────────────────────

func TestRunOnce_TrustScore_TwoDimensionalSignal(t *testing.T) {
	makeEvents := func(agentKey string, vs, bc float64) []domain.TaskEvent {
		return []domain.TaskEvent{
			{AgentID: agentKey, SourceHash: "h", Verified: true, VerifierScore: vs, BidConfidence: bc},
			{AgentID: agentKey, SourceHash: "h", Verified: true, VerifierScore: vs, BidConfidence: bc},
			{AgentID: agentKey, SourceHash: "h", Verified: true, VerifierScore: vs, BidConfidence: bc},
		}
	}
	reader := &mockTaskEventReader{
		events: map[string][]domain.TaskEvent{
			"excellent:h": makeEvents("excellent", 0.9, 0.9),
			"mediocre:h":  makeEvents("mediocre", 0.4, 0.4),
		},
		agentKeys: []string{"excellent:h", "mediocre:h"},
	}
	store := &mockAggregatorStore{}
	cfg := AggregatorConfig{
		EWMAAlpha:           0.5,
		LatencyWindow:       50,
		TrustScoreCalWeight: 0.6,
		TrustScoreAbsWeight: 0.4,
	}
	agg := New(reader, store, cfg)

	if err := agg.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	scores := map[string]float64{}
	for _, s := range store.savedProfiles {
		scores[s.AgentID] = s.Profile.TrustScore
	}
	if scores["excellent"] <= scores["mediocre"] {
		t.Errorf("excellent agent TrustScore (%.4f) should exceed mediocre (%.4f)",
			scores["excellent"], scores["mediocre"])
	}
}

func TestRunOnce_MinVerifiedEvents_KeepsNeutralWhileProvisional(t *testing.T) {
	events := []domain.TaskEvent{
		{AgentID: "a", SourceHash: "h", Verified: true, VerifierScore: 0.9, BidConfidence: 0.5},
		{AgentID: "a", SourceHash: "h", Verified: true, VerifierScore: 0.9, BidConfidence: 0.5},
	}
	reader := &mockTaskEventReader{
		events:    map[string][]domain.TaskEvent{"a:h": events},
		agentKeys: []string{"a:h"},
	}
	store := &mockAggregatorStore{
		profileToReturn: &domain.AgentProfile{AgentID: "a", SourceHash: "h", Provisional: true},
	}
	cfg := AggregatorConfig{EWMAAlpha: 0.5, LatencyWindow: 50, MinVerifiedEvents: 3}
	agg := New(reader, store, cfg)

	if err := agg.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	saved := store.savedProfiles[0].Profile
	if abs64(saved.TrustScore-0.5) > 1e-9 {
		t.Errorf("TrustScore=%.4f, want 0.5 (gate not yet crossed)", saved.TrustScore)
	}
	if !saved.Provisional {
		t.Error("Provisional must remain true below min_verified_events")
	}
}

func TestRunOnce_MinVerifiedEvents_ClearsProvisionalAfterGate(t *testing.T) {
	events := []domain.TaskEvent{
		{AgentID: "b", SourceHash: "h", Verified: true, VerifierScore: 0.8, BidConfidence: 0.8},
		{AgentID: "b", SourceHash: "h", Verified: true, VerifierScore: 0.8, BidConfidence: 0.8},
		{AgentID: "b", SourceHash: "h", Verified: true, VerifierScore: 0.8, BidConfidence: 0.8},
	}
	reader := &mockTaskEventReader{
		events:    map[string][]domain.TaskEvent{"b:h": events},
		agentKeys: []string{"b:h"},
	}
	store := &mockAggregatorStore{
		profileToReturn: &domain.AgentProfile{AgentID: "b", SourceHash: "h", Provisional: true},
	}
	cfg := AggregatorConfig{EWMAAlpha: 0.5, LatencyWindow: 50, MinVerifiedEvents: 3}
	agg := New(reader, store, cfg)

	if err := agg.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	saved := store.savedProfiles[0].Profile
	if saved.Provisional {
		t.Error("Provisional must be cleared once min_verified_events is reached")
	}
	if saved.TrustScore == 0.5 {
		t.Error("TrustScore should be computed from formula, not held at neutral, after gate is crossed")
	}
}
