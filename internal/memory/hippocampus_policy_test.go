package memory

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// testPolicyProvider is a simple PolicyProvider for hippocampus tests.
type testPolicyProvider struct {
	policies    map[string]domain.HippocampusPolicy
	defaultName string
}

func (p *testPolicyProvider) GetPolicy(name string) (domain.HippocampusPolicy, bool) {
	pol, ok := p.policies[name]
	return pol, ok
}
func (p *testPolicyProvider) DefaultPolicy() domain.HippocampusPolicy {
	return p.policies[p.defaultName]
}

// searchResultWithAge returns a SearchResult with a stored_at timestamp set to
// `age` ago, allowing MaxAgeHours tests without real time.Sleep.
func searchResultWithAge(score, confidence float64, age time.Duration) domain.SearchResult {
	plan := &domain.ExecutionPlan{Subject: "test plan"}
	envelope := ProceduralTemplateV1{Version: 1, Plan: plan}
	b, _ := json.Marshal(envelope)
	storedAt := time.Now().UTC().Add(-age).Format(time.RFC3339)
	return domain.SearchResult{
		Score: score,
		Document: domain.Document{
			Text: string(b),
			Metadata: map[string]interface{}{
				"mean_auction_confidence": confidence,
				"stored_at":               storedAt,
			},
		},
	}
}

func fivePolicy() *testPolicyProvider {
	return &testPolicyProvider{
		policies: map[string]domain.HippocampusPolicy{
			"codegen":   {SimilarityThreshold: 0.92, ConfidenceFloor: 0.85, MaxAgeHours: 24},
			"cognitive": {SimilarityThreshold: 0.85, ConfidenceFloor: 0.70, MaxAgeHours: 168},
			"tool":      {SimilarityThreshold: 0.80, ConfidenceFloor: 0.60, MaxAgeHours: 720},
			"research":  {SimilarityThreshold: 0.88, ConfidenceFloor: 0.75, MaxAgeHours: 72},
			"default":   {SimilarityThreshold: 0.85, ConfidenceFloor: 0.70, MaxAgeHours: 168},
		},
		defaultName: "default",
	}
}

// Cycle 1 — codegen policy (threshold 0.92): score 0.90 is a miss.
func TestHippocampus_RetrieveWithPolicy_Codegen_StricterThreshold(t *testing.T) {
	store := &fakeHippStore{results: []domain.SearchResult{searchResultWithAge(0.90, 0.90, time.Hour)}}
	h := NewHippocampus(store, defaultHippEmbedder(), fivePolicy())

	plan, _, _, err := h.RetrieveWithPolicy(context.Background(), "write a sorting function", "codegen")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan != nil {
		t.Error("score 0.90 should be a miss for codegen policy (threshold 0.92)")
	}
}

// Cycle 2 — cognitive policy (threshold 0.85): score 0.90 is a hit.
func TestHippocampus_RetrieveWithPolicy_Cognitive_StandardThreshold(t *testing.T) {
	store := &fakeHippStore{results: []domain.SearchResult{searchResultWithAge(0.90, 0.80, time.Hour)}}
	h := NewHippocampus(store, defaultHippEmbedder(), fivePolicy())

	plan, sim, _, err := h.RetrieveWithPolicy(context.Background(), "analyse the Q3 report", "cognitive")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan == nil {
		t.Fatal("score 0.90 should be a hit for cognitive policy (threshold 0.85)")
	}
	if sim != 0.90 {
		t.Errorf("expected similarity 0.90, got %v", sim)
	}
}

// Cycle 3 — tool policy (threshold 0.80): score 0.82 is a hit.
func TestHippocampus_RetrieveWithPolicy_Tool_RelaxedThreshold(t *testing.T) {
	store := &fakeHippStore{results: []domain.SearchResult{searchResultWithAge(0.82, 0.65, time.Hour)}}
	h := NewHippocampus(store, defaultHippEmbedder(), fivePolicy())

	plan, _, _, err := h.RetrieveWithPolicy(context.Background(), "read data/users.csv", "tool")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan == nil {
		t.Fatal("score 0.82 should be a hit for tool policy (threshold 0.80)")
	}
}

// Cycle 4 — unknown policy name falls back to default (0.85 threshold).
func TestHippocampus_RetrieveWithPolicy_UnknownPolicy_FallsBackToDefault(t *testing.T) {
	// Score 0.87 would be a hit for default (0.85) but not codegen (0.92).
	store := &fakeHippStore{results: []domain.SearchResult{searchResultWithAge(0.87, 0.75, time.Hour)}}
	h := NewHippocampus(store, defaultHippEmbedder(), fivePolicy())

	plan, _, _, err := h.RetrieveWithPolicy(context.Background(), "do something", "nonexistent_policy")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan == nil {
		t.Fatal("unknown policy should fall back to default (threshold 0.85); score 0.87 is a hit")
	}
}

// Cycle 5 — MaxAgeHours: template older than policy limit is a miss.
func TestHippocampus_RetrieveWithPolicy_MaxAgeHours_Expired(t *testing.T) {
	// codegen MaxAgeHours=24; store template 25h old.
	store := &fakeHippStore{results: []domain.SearchResult{searchResultWithAge(0.95, 0.90, 25*time.Hour)}}
	h := NewHippocampus(store, defaultHippEmbedder(), fivePolicy())

	plan, _, _, err := h.RetrieveWithPolicy(context.Background(), "generate code", "codegen")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan != nil {
		t.Error("template older than MaxAgeHours should be treated as a miss")
	}
}

// Cycle 6 — MaxAgeHours: fresh template within limit is a hit.
func TestHippocampus_RetrieveWithPolicy_MaxAgeHours_Fresh(t *testing.T) {
	// codegen MaxAgeHours=24; store template 1h old.
	store := &fakeHippStore{results: []domain.SearchResult{searchResultWithAge(0.95, 0.90, time.Hour)}}
	h := NewHippocampus(store, defaultHippEmbedder(), fivePolicy())

	plan, _, _, err := h.RetrieveWithPolicy(context.Background(), "generate code", "codegen")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan == nil {
		t.Fatal("fresh template within MaxAgeHours should be a hit")
	}
}

// Cycle 7 — Retrieve (no policy arg) has unchanged backward-compatible behaviour.
func TestHippocampus_Retrieve_BackwardCompat_WithPolicyProvider(t *testing.T) {
	// Score 0.87 hits default threshold 0.85 → should still be a hit.
	store := &fakeHippStore{results: []domain.SearchResult{searchResultWithAge(0.87, 0.75, time.Hour)}}
	h := NewHippocampus(store, defaultHippEmbedder(), fivePolicy())

	plan, _, _, err := h.Retrieve(context.Background(), "do something")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan == nil {
		t.Fatal("Retrieve should delegate to RetrieveWithPolicy with default policy")
	}
}
