package llmusage

import (
	"context"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

func TestFormat_SanitizesPurpose(t *testing.T) {
	t.Parallel()
	_, tally := domain.WithLLMUsageTally(context.Background())
	tally.Add("a,b=c.d", domain.LLMUsage{Calls: 1, CostUSD: 0.25})
	got := Format(tally)
	if !strings.Contains(got, ",a_b_c_d.calls#=1,") || !strings.HasSuffix(got, "a_b_c_d.cost_usd=0.25") {
		t.Fatalf("Format = %q", got)
	}
}

// ENG-126: the trailer reports cache writes beside cache reads.
func TestFormat_ReportsCacheReadsAndWrites(t *testing.T) {
	t.Parallel()
	_, tally := domain.WithLLMUsageTally(context.Background())
	tally.Add("agent_step", domain.LLMUsage{Calls: 2, InputTokens: 2080, CacheReadTokens: 1000, CacheWriteTokens: 1000})
	got := Format(tally)
	if !strings.HasPrefix(got, "calls#=2,input#=2080,output#=0,cache_read#=1000,cache_write#=1000,estimated#=0,") ||
		!strings.Contains(got, ",agent_step.cache_write#=1000,") {
		t.Fatalf("Format = %q", got)
	}
}
