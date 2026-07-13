package memory

import (
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// Cycle 1: all-empty inputs → empty string (tracer bullet).
func TestBuildLTMContext_AllEmpty(t *testing.T) {
	out := BuildLTMContext(nil, domain.LTMEnrichment{})
	if out != "" {
		t.Errorf("BuildLTMContext(nil, empty) = %q, want empty string", out)
	}
}

// Cycle 2: facts only → <FactLTM> section present, no other sections.
func TestBuildLTMContext_FactsOnly(t *testing.T) {
	enrichment := domain.LTMEnrichment{
		Facts: []domain.SearchResult{
			{
				Document: domain.Document{
					ID:                 "d1",
					Text:               "The Sieve of Eratosthenes runs in O(n log log n).",
					ActivationStrength: 0.72,
				},
				Score: 0.9,
			},
		},
	}
	out := BuildLTMContext(nil, enrichment)

	if !strings.Contains(out, "<FactLTM>") {
		t.Error("output must contain <FactLTM>")
	}
	if !strings.Contains(out, "Sieve of Eratosthenes") {
		t.Error("output must contain fact text")
	}
	if strings.Contains(out, "<PlanLTM") {
		t.Error("output must NOT contain <PlanLTM> when plan is nil")
	}
	if strings.Contains(out, "<NegativeLTM>") {
		t.Error("output must NOT contain <NegativeLTM> when no negatives")
	}
}

// Cycle 3: negatives only → <NegativeLTM> section present.
func TestBuildLTMContext_NegativesOnly(t *testing.T) {
	enrichment := domain.LTMEnrichment{
		Negatives: []domain.SearchResult{
			{
				Document: domain.Document{
					ID:   "n1",
					Text: "BLOCKED: 'write' is not in ALLOWED_COMMANDS",
					Metadata: map[string]interface{}{
						"agent_id": "terminal_agent",
					},
				},
			},
		},
	}
	out := BuildLTMContext(nil, enrichment)

	if !strings.Contains(out, "<NegativeLTM>") {
		t.Error("output must contain <NegativeLTM>")
	}
	if !strings.Contains(out, "terminal_agent") {
		t.Error("output must contain agent attribute from negative edge metadata")
	}
	if strings.Contains(out, "<FactLTM>") {
		t.Error("output must NOT contain <FactLTM> when no facts")
	}
}

// Cycle 4: plan only → <PlanLTM> tag with attributes.
func TestBuildLTMContext_PlanOnly(t *testing.T) {
	plan := &domain.PlanLTMEntry{
		PlanJSON:    `{"subject":"prime numbers","steps":[]}`,
		Similarity:  0.87,
		Confidence:  0.85,
		Outcome:     "success",
		ReplanCount: 0,
	}
	out := BuildLTMContext(plan, domain.LTMEnrichment{})

	if !strings.Contains(out, "<PlanLTM") {
		t.Error("output must contain <PlanLTM")
	}
	if !strings.Contains(out, `similarity="0.87"`) {
		t.Error("output must contain similarity attribute")
	}
	if !strings.Contains(out, `outcome="success"`) {
		t.Error("output must contain outcome attribute")
	}
	if !strings.Contains(out, `replan_count="0"`) {
		t.Error("output must contain replan_count attribute")
	}
	if !strings.Contains(out, "prime numbers") {
		t.Error("output must contain plan JSON content")
	}
}

// Cycle 5: all three → all three sections present.
func TestBuildLTMContext_AllThreeSections(t *testing.T) {
	plan := &domain.PlanLTMEntry{PlanJSON: `{}`, Similarity: 0.9, Confidence: 0.8, Outcome: "success"}
	enrichment := domain.LTMEnrichment{
		Facts: []domain.SearchResult{
			{Document: domain.Document{ID: "f1", Text: "a fact", ActivationStrength: 0.5}, Score: 0.8},
		},
		Negatives: []domain.SearchResult{
			{Document: domain.Document{ID: "n1", Text: "BLOCKED: cmd",
				Metadata: map[string]interface{}{"agent_id": "terminal_agent"}}},
		},
	}
	out := BuildLTMContext(plan, enrichment)

	for _, section := range []string{"<PlanLTM", "<FactLTM>", "<NegativeLTM>"} {
		if !strings.Contains(out, section) {
			t.Errorf("output must contain %s", section)
		}
	}
}

// Cycle 6: fact elements carry id and activation attributes.
func TestBuildLTMContext_FactAttributes(t *testing.T) {
	enrichment := domain.LTMEnrichment{
		Facts: []domain.SearchResult{
			{Document: domain.Document{ID: "f1", Text: "fact zero", ActivationStrength: 0.72}, Score: 0.9},
			{Document: domain.Document{ID: "f2", Text: "fact one", ActivationStrength: 0.45}, Score: 0.8},
		},
	}
	out := BuildLTMContext(nil, enrichment)

	if !strings.Contains(out, `id="0"`) {
		t.Error("first fact must have id=0")
	}
	if !strings.Contains(out, `id="1"`) {
		t.Error("second fact must have id=1")
	}
	if !strings.Contains(out, `activation="0.72"`) {
		t.Error("first fact must have activation=0.72")
	}
}
