package llm

import (
	"context"
	"net/http"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// ENG-453 (owner decision 2026-11-14): benchmark and pinned kernels never fall
// back and fail fast; production falls back only to generators an operator marks
// equivalent (llm_provider.equivalent_generators). The marked-equivalent walk is
// exercised by the existing fallback tests (twoGeneratorProvider, sseProvider).

func policyProvider(t *testing.T, a, b *countingServer, pinned bool, groups [][]string) *Provider {
	t.Helper()
	p, err := NewProvider(config.LLMProviderConfig{
		Default: "a",
		Generators: []config.GeneratorConfig{
			{ID: "a", Provider: "openai", Model: "m", Endpoint: a.srv.URL, TimeoutMs: 5000},
			{ID: "b", Provider: "openai", Model: "m", Endpoint: b.srv.URL, TimeoutMs: 5000},
		},
		Roles:                map[string]string{"planner": "a"},
		Health:               config.HealthConfig{FailureThreshold: 5, CooldownMs: 60000},
		Pinned:               pinned,
		EquivalentGenerators: groups,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

const quota429 = `{"error":{"type":"insufficient_quota","message":"quota exceeded"}}`

func TestFallbackPolicy_UnmarkedGeneratorNeverServes(t *testing.T) {
	t.Parallel()
	a := respondServer(t, http.StatusTooManyRequests, quota429)
	b := respondServer(t, http.StatusOK, okBody(`"from b"`, "stop"))
	p := policyProvider(t, a, b, false, nil)

	// Organ path (purpose ladder).
	if out, err := p.GeneratorFor(domain.PurposePlanner).Generate(context.Background(), "plan"); err == nil || out == "from b" {
		t.Fatalf("Generate = %q, %v; want a's own failure, never b's answer", out, err)
	}
	// Agent-plane path (a step's candidate chain).
	if _, served, err := p.GenerateOn(context.Background(), []string{"a", "b"}, domain.UserPrompt("x")); err == nil || served == "b" {
		t.Fatalf("GenerateOn served by %q, %v; want a's own failure", served, err)
	}
	if b.hits.Load() != 0 {
		t.Fatalf("b was called %d times; it is not marked equivalent to a", b.hits.Load())
	}
}

func TestFallbackPolicy_PinnedNeverFallsBackEvenToAnEquivalent(t *testing.T) {
	t.Parallel()
	a := respondServer(t, http.StatusTooManyRequests, quota429)
	b := respondServer(t, http.StatusOK, okBody(`"from b"`, "stop"))
	p := policyProvider(t, a, b, true, [][]string{{"a", "b"}})

	if _, err := p.GeneratorFor(domain.PurposePlanner).Generate(context.Background(), "plan"); err == nil {
		t.Fatal("pinned: want a's failure")
	}
	if _, _, err := p.GenerateOn(context.Background(), []string{"a", "b"}, domain.UserPrompt("x")); err == nil {
		t.Fatal("pinned: GenerateOn want a's failure")
	}
	if _, _, err := p.StreamOn(context.Background(), []string{"a", "b"}, domain.UserPrompt("x")); err == nil {
		t.Fatal("pinned: StreamOn want a's failure")
	}
	if b.hits.Load() != 0 {
		t.Fatalf("pinned kernel called b %d times", b.hits.Load())
	}
}

// Pinned: an OPEN breaker on the first choice fails the call at once (fail fast),
// it does not resolve to the default or a capability match.
func TestFallbackPolicy_PinnedUnhealthyFirstChoiceFailsFast(t *testing.T) {
	t.Parallel()
	a := respondServer(t, http.StatusOK, okBody(`"from a"`, "stop"))
	b := respondServer(t, http.StatusOK, okBody(`"from b"`, "stop"))
	p := policyProvider(t, a, b, true, nil)
	p.SetRole("planner", "b")
	for i := 0; i < 5; i++ {
		p.breaker.Record(context.Background(), "b", false)
	}
	if id, err := p.resolve(domain.LLMRequest{Purpose: domain.PurposePlanner}); err == nil {
		t.Fatalf("resolved %q; the pinned first choice b is open, nothing else may serve", id)
	}
}

// A healthy first choice is served exactly as before, pinned or not.
func TestFallbackPolicy_FirstChoiceServesUnchanged(t *testing.T) {
	t.Parallel()
	a := respondServer(t, http.StatusOK, okBody(`"from a"`, "stop"))
	b := respondServer(t, http.StatusOK, okBody(`"from b"`, "stop"))
	for _, pinned := range []bool{false, true} {
		p := policyProvider(t, a, b, pinned, nil)
		out, err := p.GeneratorFor(domain.PurposePlanner).Generate(context.Background(), "plan")
		if err != nil || out != "from a" {
			t.Fatalf("pinned=%v: Generate = %q, %v", pinned, out, err)
		}
		// A role naming an unknown generator still resolves to the default: that
		// is config tolerance (the default IS the call's first choice), not fallback.
		p.SetRole("planner", "nope")
		if id, err := p.resolve(domain.LLMRequest{Purpose: domain.PurposePlanner}); err != nil || id != "a" {
			t.Fatalf("pinned=%v: unknown role resolves to %q, %v; want the default a", pinned, id, err)
		}
	}
}

// Equivalence is "shares a group", not closed over shared members.
func TestFallbackPolicy_EquivalenceIsSharedGroupMembership(t *testing.T) {
	fp := newFallbackPolicy(false, [][]string{{"a", "b"}, {"b", "c"}})
	for _, c := range []struct {
		first, id string
		want      bool
	}{
		{"a", "a", true}, {"a", "b", true}, {"b", "a", true}, {"b", "c", true},
		{"a", "c", false}, {"c", "a", false}, {"a", "z", false}, {"", "z", true},
	} {
		if got := fp.admits(c.first, c.id); got != c.want {
			t.Errorf("admits(%q,%q) = %v, want %v", c.first, c.id, got, c.want)
		}
	}
	if newFallbackPolicy(true, [][]string{{"a", "b"}}).admits("a", "b") {
		t.Error("pinned admits a fallback")
	}
}
