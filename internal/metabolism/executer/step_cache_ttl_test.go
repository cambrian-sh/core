package executer

import (
	"testing"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// Cycle 1 — IsThought=true → 1 hour.
func TestResolveCacheTTL_ThoughtStep_OneHour(t *testing.T) {
	s := domain.Step{IsThought: true}
	if got := resolveCacheTTL(s, nil); got != time.Hour {
		t.Errorf("expected 1h for thought step, got %v", got)
	}
}

// Cycle 2 — code-gen keywords → 0 (never cache).
func TestResolveCacheTTL_CodeGenKeywords_Zero(t *testing.T) {
	for _, q := range []string{
		"write a function to sort users",
		"implement the payment handler",
		"generate code for the auth module",
	} {
		s := domain.Step{Query: q}
		if got := resolveCacheTTL(s, nil); got != 0 {
			t.Errorf("query %q: expected TTL=0 (never cache), got %v", q, got)
		}
	}
}

// Cycle 3 — RecommendedModel set → 24 hours.
func TestResolveCacheTTL_RecommendedModel_TwentyFourHours(t *testing.T) {
	s := domain.Step{RecommendedModel: "claude-opus-4-5"}
	if got := resolveCacheTTL(s, nil); got != 24*time.Hour {
		t.Errorf("expected 24h for cognitive step, got %v", got)
	}
}

// Cycle 4 — default (tool/data step) → 7 days.
func TestResolveCacheTTL_Default_SevenDays(t *testing.T) {
	s := domain.Step{Query: "read data/users.csv and return row count"}
	if got := resolveCacheTTL(s, nil); got != 7*24*time.Hour {
		t.Errorf("expected 7d for default step, got %v", got)
	}
}

// Cycle 5 — explicit CacheTTLSeconds always wins (even over code-gen keywords).
func TestResolveCacheTTL_ExplicitTTL_Wins(t *testing.T) {
	s := domain.Step{
		Query:           "write some code",
		CacheTTLSeconds: 3600,
	}
	if got := resolveCacheTTL(s, nil); got != time.Hour {
		t.Errorf("expected explicit 1h TTL to win over heuristic, got %v", got)
	}
}

// Cycle 6 — IsThought takes priority over RecommendedModel.
func TestResolveCacheTTL_ThoughtBeatsRecommendedModel(t *testing.T) {
	s := domain.Step{IsThought: true, RecommendedModel: "claude-opus-4-5"}
	if got := resolveCacheTTL(s, nil); got != time.Hour {
		t.Errorf("IsThought should yield 1h even when RecommendedModel is set, got %v", got)
	}
}
