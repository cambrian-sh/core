package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// The per-request tally is fed by the meter at the Acquire door: an organ call
// made under a ctx carrying a domain.LLMUsageTally is counted there under its
// purpose with the provider's usage — and a call made on a detached ctx
// (context.WithoutCancel, the async fact job's shape) still reaches it.
func TestSpendMeter_FeedsTheRequestTallyByPurpose(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"facts"}}],` +
			`"usage":{"prompt_tokens":200,"completion_tokens":50,"total_tokens":250}}`))
	}))
	defer srv.Close()

	p := httpProvider(t, srv.URL, "openai", 1.0, 2.0)
	facts := p.GeneratorFor(domain.PurposeFacts)
	meter := NewSpendMeter(0, p.Ledger(), nil)
	p.SetSpendMeter(meter)

	ctx, tally := domain.WithLLMUsageTally(context.Background())
	if _, err := facts.Generate(ctx, "extract"); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, err := facts.Generate(context.WithoutCancel(ctx), "assess"); err != nil {
		t.Fatalf("Generate (detached): %v", err)
	}
	if _, err := facts.Generate(context.Background(), "someone else's call"); err != nil {
		t.Fatalf("Generate (no tally): %v", err)
	}

	got := tally.ByPurpose()
	if len(got) != 1 || got[0].Purpose != string(domain.PurposeFacts) {
		t.Fatalf("tally purposes = %+v, want facts only", got)
	}
	u := got[0].Usage
	if u.Calls != 2 || u.InputTokens != 400 || u.OutputTokens != 100 || u.EstimatedCalls != 0 {
		t.Fatalf("facts usage = %+v, want 2 calls 400/100, provider-reported", u)
	}
	// 2 x (200 * $1/1M + 50 * $2/1M) = 0.0006
	if u.CostUSD < 0.0006-1e-12 || u.CostUSD > 0.0006+1e-12 {
		t.Fatalf("cost = %v, want 0.0006", u.CostUSD)
	}
	// The ledger saw all three calls (the third in the no-session bucket).
	if n := meter.NoSession().Calls; n != 3 {
		t.Fatalf("ledger no-session calls = %d, want 3", n)
	}
}

// A call booked to an unsettled SpendTab (router classification before the
// session exists) is still counted on the request's tally.
func TestSpendMeter_TabCallsReachTheTally(t *testing.T) {
	t.Parallel()
	meter := NewSpendMeter(0, nil, nil)
	ctx, tally := domain.WithLLMUsageTally(context.Background())
	ctx, tab := WithSpendTab(ctx)
	meter.Record(ctx, "router", "", TokenUsage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10})
	if tab.Spend().Calls != 1 {
		t.Fatalf("tab = %+v, want the call", tab.Spend())
	}
	if got := tally.Total(); got.Calls != 1 || got.InputTokens != 7 || got.OutputTokens != 3 {
		t.Fatalf("tally = %+v, want the tab's call", got)
	}
}

// The gateway door passes the lease's tally explicitly.
func TestSpendMeter_RecordSessionTally(t *testing.T) {
	t.Parallel()
	meter := NewSpendMeter(0, nil, nil)
	_, tally := domain.WithLLMUsageTally(context.Background())
	meter.RecordSessionTally("ses-t", tally, "agent_step:retrieval_agent", "", TokenUsage{PromptTokens: 5, Estimated: true})
	meter.RecordSessionTally("ses-t", nil, "agent_step", "", TokenUsage{PromptTokens: 5})
	if got := tally.Total(); got.Calls != 1 || got.EstimatedCalls != 1 {
		t.Fatalf("tally = %+v, want the one tallied call, flagged estimated", got)
	}
	if got := meter.Session("ses-t"); got.Calls != 2 {
		t.Fatalf("session = %+v, want both calls in the ledger", got)
	}
}
