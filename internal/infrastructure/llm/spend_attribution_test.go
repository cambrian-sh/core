package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// Spend attribution properties on the one meter (ADR-0131; owner decision
// 2026-11-01, T-20261101-router-classification-and-async-organs). Every test
// goes through a REAL acquired organ generator, so the properties are proven on
// the decorator every organ call passes, not on the meter's API alone.

func usageServer(t *testing.T, hits *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],` +
			`"usage":{"prompt_tokens":60,"completion_tokens":20,"total_tokens":80}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// (b) A background organ call carrying its originating session is booked to
// that session and is NOT refused although the session is over its ceiling;
// the same call on the request path is refused.
func TestSpendMeter_BackgroundOrganBookedToSessionNeverRefused(t *testing.T) {
	t.Parallel()
	var hits int32
	p := httpProvider(t, usageServer(t, &hits).URL, "openai", 0, 0)
	meter := NewSpendMeter(100, nil, nil)
	p.SetSpendMeter(meter)
	verifier := p.GeneratorFor(domain.PurposeVerifier)

	meter.RecordSession("ses-b", "planner", "", TokenUsage{PromptTokens: 150, CompletionTokens: 50, TotalTokens: 200})

	reqCtx := domain.WithSessionID(context.Background(), "ses-b")
	if _, err := verifier.Generate(reqCtx, "judge"); !errors.Is(err, ErrSpendCeiling) {
		t.Fatalf("request-path call over the ceiling: want ErrSpendCeiling, got %v", err)
	}
	if hits != 0 {
		t.Fatalf("a refused call reached the provider (%d hits)", hits)
	}

	bgCtx := domain.WithBackgroundSpend(context.WithoutCancel(reqCtx))
	if _, err := verifier.Generate(bgCtx, "judge"); err != nil {
		t.Fatalf("background organ call refused: %v", err)
	}
	got := meter.Session("ses-b")
	if got.Calls != 2 || got.InputTokens != 210 || got.OutputTokens != 70 {
		t.Fatalf("session = %+v, want the background call (60/20) booked on top of 150/50", got)
	}
	if nb := meter.NoSession(); nb.Calls != 0 {
		t.Fatalf("background call with a session leaked into the no-session bucket: %+v", nb)
	}
}

// (c) A call with no originating session lands in the kernel no-session bucket,
// is never refused, and is reported once per accrual.
func TestSpendMeter_NoSessionCallGoesToKernelBucket(t *testing.T) {
	t.Parallel()
	var hits int32
	p := httpProvider(t, usageServer(t, &hits).URL, "openai", 0, 0)
	meter := NewSpendMeter(10, nil, nil) // tiny ceiling: must not apply to the bucket
	p.SetSpendMeter(meter)
	interview := p.GeneratorFor(domain.PurposeInterview)

	for i := 0; i < 2; i++ {
		if _, err := interview.Generate(context.Background(), "grade"); err != nil {
			t.Fatalf("no-session call refused: %v", err)
		}
	}
	if nb := meter.NoSession(); nb.Calls != 2 || nb.TotalTokens() != 160 || nb.Exhausted {
		t.Fatalf("%s = %+v, want 2 calls / 160 tokens, never exhausted", NoSessionBucket, nb)
	}
	if d := meter.ReportNoSession(); d.Calls != 2 {
		t.Fatalf("first report = %+v, want the 2 calls", d)
	}
	if d := meter.ReportNoSession(); d.Calls != 0 {
		t.Fatalf("second report = %+v, want nothing new", d)
	}
}

// The router's tab: enforced against its own total while the session is not
// known, settled into the session afterwards; later calls under the tab's ctx
// are the session's.
func TestSpendMeter_TabSettlesIntoSessionAndEnforcesItsOwnTotal(t *testing.T) {
	t.Parallel()
	var hits int32
	p := httpProvider(t, usageServer(t, &hits).URL, "openai", 0, 0)
	meter := NewSpendMeter(100, nil, nil)
	p.SetSpendMeter(meter)
	router := p.GeneratorFor(domain.PurposeRouter)

	ctx, tab := WithSpendTab(context.Background())
	if _, err := router.Generate(ctx, "classify"); err != nil {
		t.Fatalf("first tab call: %v", err)
	}
	if got := tab.Spend(); got.Calls != 1 || got.TotalTokens() != 80 {
		t.Fatalf("tab = %+v", got)
	}
	if nb := meter.NoSession(); nb.Calls != 0 {
		t.Fatalf("tab spend leaked into the no-session bucket: %+v", nb)
	}
	// 80 used + a ~30-token prompt estimate > 100: refused on the tab's own total.
	if _, err := router.Generate(ctx, string(make([]byte, 120))); !errors.Is(err, ErrSpendCeiling) {
		t.Fatalf("tab over the ceiling: want ErrSpendCeiling, got %v", err)
	}

	meter.SettleTab(tab, "ses-t")
	meter.SettleTab(tab, "ses-other") // idempotent
	got := meter.Session("ses-t")
	if got.Calls != 1 || got.TotalTokens() != 80 || !got.Exhausted {
		t.Fatalf("settled session = %+v, want the tab's call and its exhaustion", got)
	}
	if meter.Session("ses-other").Calls != 0 {
		t.Fatal("a second settle must not move the tab again")
	}
}

// A tab that never gets a session (a CHAT turn) settles into the no-session
// bucket, which is never enforced.
func TestSpendMeter_UnclaimedTabSettlesIntoKernelBucket(t *testing.T) {
	t.Parallel()
	meter := NewSpendMeter(50, nil, nil)
	ctx, tab := WithSpendTab(context.Background())
	meter.Record(ctx, "router", "", TokenUsage{PromptTokens: 40, CompletionTokens: 20, TotalTokens: 60})
	_ = meter.Admit(ctx, "router", 10) // over: marks the tab exhausted
	meter.SettleTab(tab, "")
	if nb := meter.NoSession(); nb.Calls != 1 || nb.TotalTokens() != 60 || nb.Exhausted {
		t.Fatalf("bucket = %+v, want the tab's call, not exhausted", nb)
	}
	if err := meter.Admit(context.Background(), "interview", 1000); err != nil {
		t.Fatalf("the no-session bucket must never refuse: %v", err)
	}
}
