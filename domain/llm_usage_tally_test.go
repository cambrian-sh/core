package domain

import (
	"context"
	"sync"
	"testing"
)

func TestLLMUsageTally_SplitsByPurposeAndForwardsToParent(t *testing.T) {
	t.Parallel()
	ctx, outer := WithLLMUsageTally(context.Background())
	ictx, inner := WithLLMUsageTally(ctx)
	if LLMUsageTallyFrom(ictx) != inner {
		t.Fatal("inner ctx must carry the inner tally")
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			inner.Add("facts", LLMUsage{Calls: 1, InputTokens: 100, OutputTokens: 10, CostUSD: 0.5})
		}()
	}
	wg.Wait()
	outer.Add("", LLMUsage{Calls: 1, EstimatedCalls: 1})

	if got := inner.Total(); got.Calls != 10 || got.InputTokens != 1000 || got.OutputTokens != 100 || got.CostUSD != 5 {
		t.Fatalf("inner total = %+v", got)
	}
	got := outer.ByPurpose()
	if len(got) != 2 || got[0].Purpose != "facts" || got[0].Usage.Calls != 10 || got[1].Purpose != "model" || got[1].Usage.EstimatedCalls != 1 {
		t.Fatalf("outer by purpose = %+v, want facts (forwarded from inner) + model", got)
	}
}

func TestLLMUsageTally_NilSafe(t *testing.T) {
	t.Parallel()
	var tally *LLMUsageTally
	tally.Add("facts", LLMUsage{Calls: 1})
	if tally.Total().Calls != 0 || tally.ByPurpose() != nil {
		t.Fatal("nil tally must be a no-op")
	}
	if LLMUsageTallyFrom(context.Background()) != nil {
		t.Fatal("no tally on a bare ctx")
	}
}
