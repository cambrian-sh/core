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
