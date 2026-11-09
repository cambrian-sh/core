package executer

import (
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// Cycle 1 — IsThought=true → 1 hour.
func TestResolveCacheTTL_ThoughtStep_OneHour(t *testing.T) {
	s := domain.Step{IsThought: true}
	if got := resolveCacheTTL(s, nil, 0); got != time.Hour {
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
		if got := resolveCacheTTL(s, nil, 0); got != 0 {
			t.Errorf("query %q: expected TTL=0 (never cache), got %v", q, got)
		}
	}
}

// Cycle 4 — default (tool/data step) → one hour when no default is configured
// (was 7 days before ADR-0145 D9).
func TestResolveCacheTTL_Default_OneHour(t *testing.T) {
	s := domain.Step{Query: "read data/users.csv and return row count"}
	if got := resolveCacheTTL(s, nil, 0); got != time.Hour {
		t.Errorf("expected 1h for default step, got %v", got)
	}
}

// Cycle 5 — explicit CacheTTLSeconds always wins (even over code-gen keywords).
func TestResolveCacheTTL_ExplicitTTL_Wins(t *testing.T) {
	s := domain.Step{
		Query:           "write some code",
		CacheTTLSeconds: 3600,
	}
	if got := resolveCacheTTL(s, nil, 0); got != time.Hour {
		t.Errorf("expected explicit 1h TTL to win over heuristic, got %v", got)
	}
}

// The configured default (execution.plan.step_cache_default_ttl_minutes) applies
// when no policy names the step; an explicit policy still wins.
func TestResolveCacheTTL_ConfiguredDefault(t *testing.T) {
	s := domain.Step{Query: "list the invoices"}
	if got := resolveCacheTTL(s, nil, 15*time.Minute); got != 15*time.Minute {
		t.Errorf("configured default: got %v, want 15m", got)
	}
	if got := resolveCacheTTL(s, map[string]int{"tool": 2}, 15*time.Minute); got != 2*time.Hour {
		t.Errorf("policy must beat the default: got %v, want 2h", got)
	}
}
