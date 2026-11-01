package llm

import (
	"testing"

	"github.com/cambrian-sh/core/domain"
)

func TestStepUsage_RoundTripAndAgentValuesCleared(t *testing.T) {
	t.Parallel()
	// An agent echoing the keys must not survive the stamp.
	ctx := map[string]string{domain.HandoffKeyStepPromptTokens: "999999", domain.HandoffKeyStepUsageEstimated: "true"}
	StampStepUsage(ctx, TokenUsage{PromptTokens: 120, CompletionTokens: 30, TotalTokens: 150, CacheReadTokens: 100})
	u, ok := StepUsageFrom(ctx)
	if !ok || u.PromptTokens != 120 || u.CompletionTokens != 30 || u.TotalTokens != 150 || u.CacheReadTokens != 100 || u.Estimated {
		t.Fatalf("round trip = %+v ok=%v", u, ok)
	}
	// Zero usage stamps nothing (and still clears an agent value).
	StampStepUsage(ctx, TokenUsage{})
	if _, ok := StepUsageFrom(ctx); ok || len(ctx) != 0 {
		t.Fatalf("zero usage must leave no keys: %v", ctx)
	}
	StampStepUsage(ctx, TokenUsage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2, Estimated: true})
	if u, _ := StepUsageFrom(ctx); !u.Estimated {
		t.Fatal("an estimated lease usage must stay flagged")
	}
	StampStepUsage(nil, TokenUsage{PromptTokens: 1}) // nil-safe
}
