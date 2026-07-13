package router_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/router"
	"github.com/cambrian-sh/core/internal/testing/harness"
)

func defaultExecConfig() config.ExecutionConfig {
	return config.DefaultConfig().Execution
}

// ── Layer 3 — LLM classification ─────────────────────────────────────────────

const highConfidenceResponse = `{"decision":"plan","confidence":0.92,"alternatives":[{"decision":"chat","confidence":0.05}],"reason":"execution intent"}`
const lowConfidenceResponse = `{"decision":"plan","confidence":0.35,"alternatives":[{"decision":"chat","confidence":0.50},{"decision":"ingest","confidence":0.08}],"reason":"ambiguous"}`
const tinyAlternativesResponse = `{"decision":"plan","confidence":0.30,"alternatives":[{"decision":"chat","confidence":0.60},{"decision":"watch","confidence":0.05}],"reason":"ambiguous"}`
const unknownDecisionResponse = `{"decision":"search","confidence":0.90,"alternatives":[],"reason":"unknown"}`
const invalidJSONResponse = `not-json`

func newRouterWithGen(gen domain.Generator) *router.DefaultRouter {
	return router.NewWithConfig(gen, 0.5, 500)
}

// Cycle 27 — High-confidence LLM response returns the classified decision.
func TestLayer3_HighConfidence_ReturnsDecision(t *testing.T) {
	gen := harness.NewFakeGenerator(highConfidenceResponse)
	r := newRouterWithGen(gen)

	dec, err := r.Resolve(context.Background(), domain.RouterInput{Body: "can you refactor this for me"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Type != domain.DecisionPlan {
		t.Fatalf("expected DecisionPlan, got %q", dec.Type)
	}
}

// Cycle 28 — Low-confidence response returns DecisionClarification.
func TestLayer3_LowConfidence_ReturnsClarification(t *testing.T) {
	gen := harness.NewFakeGenerator(lowConfidenceResponse)
	r := newRouterWithGen(gen)

	dec, err := r.Resolve(context.Background(), domain.RouterInput{Body: "help me with this"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.Type != domain.DecisionClarification {
		t.Fatalf("expected DecisionClarification, got %q", dec.Type)
	}
}

// Cycle 29 — Top candidate has Recommended=true; it is the highest-confidence
// option (even if it is the one that triggered clarification).
func TestLayer3_Clarification_TopCandidateIsRecommended(t *testing.T) {
	gen := harness.NewFakeGenerator(lowConfidenceResponse)
	r := newRouterWithGen(gen)

	dec, _ := r.Resolve(context.Background(), domain.RouterInput{Body: "help me with this"})

	if len(dec.ClarificationOptions) == 0 {
		t.Fatal("expected ClarificationOptions to be populated")
	}
	var hasRecommended bool
	for _, opt := range dec.ClarificationOptions {
		if opt.Recommended {
			hasRecommended = true
			// The recommended option should be the top Layer 3 candidate.
			if opt.Decision != domain.DecisionPlan {
				t.Errorf("expected recommended decision to be plan (top candidate), got %q", opt.Decision)
			}
		}
	}
	if !hasRecommended {
		t.Fatal("expected exactly one option with Recommended=true")
	}
}

// Cycle 30 — Alternatives with confidence >0.1 are included; ≤0.1 are dropped.
func TestLayer3_Clarification_AlternativesFilteredByThreshold(t *testing.T) {
	// tinyAlternativesResponse has: plan=0.30 (top), chat=0.60 (alt, >0.1 → included),
	// watch=0.05 (alt, ≤0.1 → dropped).
	gen := harness.NewFakeGenerator(tinyAlternativesResponse)
	r := newRouterWithGen(gen)

	dec, _ := r.Resolve(context.Background(), domain.RouterInput{Body: "help me with this"})

	// Should have plan (recommended) + chat (above threshold) = 2 options.
	// watch (0.05 ≤ 0.1) must be dropped.
	if len(dec.ClarificationOptions) != 2 {
		t.Fatalf("expected 2 options (plan + chat); watch dropped; got %d: %v",
			len(dec.ClarificationOptions), dec.ClarificationOptions)
	}
	for _, opt := range dec.ClarificationOptions {
		if opt.Decision == domain.DecisionWatch {
			t.Fatal("watch alternative (confidence 0.05) should have been dropped")
		}
	}
}

// Cycle 31 — Unknown decision string from LLM → hard error.
func TestLayer3_UnknownDecision_ReturnsError(t *testing.T) {
	gen := harness.NewFakeGenerator(unknownDecisionResponse)
	r := newRouterWithGen(gen)

	_, err := r.Resolve(context.Background(), domain.RouterInput{Body: "search for something"})
	if err == nil {
		t.Fatal("expected hard error for unknown decision string from LLM")
	}
}

// Cycle 32 — Invalid JSON from LLM → hard error.
func TestLayer3_InvalidJSON_ReturnsError(t *testing.T) {
	gen := harness.NewFakeGenerator(invalidJSONResponse)
	r := newRouterWithGen(gen)

	_, err := r.Resolve(context.Background(), domain.RouterInput{Body: "do something"})
	if err == nil {
		t.Fatal("expected hard error for invalid JSON from LLM")
	}
}

// Cycle 33 — Generator error → hard error propagated (no silent fallback).
func TestLayer3_GeneratorError_PropagatesError(t *testing.T) {
	// Empty FakeGenerator panics; use a dedicated erroring generator instead.
	r := newRouterWithGen(&errorGenerator{})

	_, err := r.Resolve(context.Background(), domain.RouterInput{Body: "do something"})
	if err == nil {
		t.Fatal("expected hard error when generator returns error")
	}
}

// Cycle 34 — Body is truncated to RouterClassificationBodyChars before prompt.
func TestLayer3_BodyTruncation(t *testing.T) {
	var capturedPrompt string
	capturingGen := &captureGenerator{
		response: highConfidenceResponse,
		capture:  func(p string) { capturedPrompt = p },
	}
	r := router.NewWithConfig(capturingGen, 0.5, 10) // 10-char truncation

	body := "this is a very long input that should be truncated before the LLM sees it"
	_, _ = r.Resolve(context.Background(), domain.RouterInput{Body: body})

	if len(capturedPrompt) == 0 {
		t.Fatal("expected a prompt to be captured")
	}
	// The truncated body (first 10 chars: "this is a ") must appear; full body must not.
	truncated := body[:10]
	if !containsString(capturedPrompt, truncated) {
		t.Errorf("expected truncated body %q to appear in prompt", truncated)
	}
	if containsString(capturedPrompt, body[10:]) {
		t.Error("full body beyond truncation limit should not appear in prompt")
	}
}

// Cycle 35 — PromptRegistry has "router.classify" entry after init().
func TestLayer3_PromptRegistry_HasClassifyEntry(t *testing.T) {
	found := false
	for _, entry := range domain.PromptRegistry {
		if entry.ID == "router.classify" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected 'router.classify' to be registered in domain.PromptRegistry")
	}
}

// ── Config defaults ───────────────────────────────────────────────────────────

// Cycle 36 — RouterMinClassificationConfidence default is 0.5.
func TestConfig_RouterMinConfidence_Default(t *testing.T) {
	cfg := defaultExecConfig()
	if cfg.RouterMinClassificationConfidence != 0.5 {
		t.Fatalf("expected default 0.5, got %v", cfg.RouterMinClassificationConfidence)
	}
}

// Cycle 37 — RouterClassificationBodyChars default is 500.
func TestConfig_RouterBodyChars_Default(t *testing.T) {
	cfg := defaultExecConfig()
	if cfg.RouterClassificationBodyChars != 500 {
		t.Fatalf("expected default 500, got %d", cfg.RouterClassificationBodyChars)
	}
}

// ── Test doubles ─────────────────────────────────────────────────────────────

type errorGenerator struct{}

func (g *errorGenerator) Generate(_ context.Context, _ string) (string, error) {
	return "", errGeneratorFailed
}

var errGeneratorFailed = fmt.Errorf("generator: simulated failure")

type captureGenerator struct {
	response string
	capture  func(string)
}

func (g *captureGenerator) Generate(_ context.Context, prompt string) (string, error) {
	if g.capture != nil {
		g.capture(prompt)
	}
	return g.response, nil
}

func containsString(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
