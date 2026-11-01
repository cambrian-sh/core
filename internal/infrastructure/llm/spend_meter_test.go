package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// ADR-0131 D1+D2 end to end through a REAL adapter: an organ generator handed
// out by GeneratorFor (before the meter was attached, as app.go does) books the
// provider's own usage — cache reads included — against the session in ctx.
func TestSpendMeter_OrganCallBooksProviderUsage(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"plan"}}],` +
			`"usage":{"prompt_tokens":1200,"completion_tokens":300,"total_tokens":1500,` +
			`"prompt_tokens_details":{"cached_tokens":1000}}}`))
	}))
	defer srv.Close()

	p := httpProvider(t, srv.URL, "openai", 2.0, 8.0)
	planner := p.GeneratorFor(domain.PurposePlanner) // created BEFORE the meter, like app.go
	meter := NewSpendMeter(0, p.Ledger(), nil)
	p.SetSpendMeter(meter)

	ctx := domain.WithSessionID(context.Background(), "ses-1")
	if _, err := planner.Generate(ctx, "make a plan"); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	got := meter.Session("ses-1")
	if got.Calls != 1 || got.InputTokens != 1200 || got.OutputTokens != 300 || got.CacheReadTokens != 1000 {
		t.Fatalf("session spend = %+v, want provider usage 1200/300 cache 1000", got)
	}
	if got.EstimatedCalls != 0 {
		t.Fatalf("provider reported usage; nothing may be flagged estimated: %+v", got)
	}
	// 1200 * $2/1M + 300 * $8/1M = 0.0024 + 0.0024
	if want := 0.0048; got.CostUSD < want-1e-9 || got.CostUSD > want+1e-9 {
		t.Fatalf("cost = %v, want %v", got.CostUSD, want)
	}
}

// Anthropic's input_tokens excludes cache reads and writes; the ledger's input
// figure is all three, with the cached part broken out.
func TestSpendMeter_AnthropicUsageCountsCachedInput(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"content":[{"text":"ok"}],"usage":{"input_tokens":50,` +
			`"output_tokens":20,"cache_read_input_tokens":900,"cache_creation_input_tokens":10}}`))
	}))
	defer srv.Close()

	p := httpProvider(t, srv.URL, "anthropic", 0, 0)
	meter := NewSpendMeter(0, nil, nil)
	p.SetSpendMeter(meter)
	ctx := domain.WithSessionID(context.Background(), "ses-a")
	if _, err := p.GeneratorFor(domain.PurposeVerifier).Generate(ctx, "verify"); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	got := meter.Session("ses-a")
	if got.InputTokens != 960 || got.OutputTokens != 20 || got.CacheReadTokens != 900 {
		t.Fatalf("anthropic spend = %+v, want input 960 (50+900+10), output 20, cache 900", got)
	}
}

// A provider that returns no usage block is booked by the len/4 fallback and the
// row is flagged — never recorded as a free call (ADR-0131 D2).
func TestSpendMeter_NoProviderUsageFallsBackFlagged(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"0123456789abcdef"}}]}`))
	}))
	defer srv.Close()

	p := httpProvider(t, srv.URL, "openai", 0, 0)
	meter := NewSpendMeter(0, nil, nil)
	p.SetSpendMeter(meter)
	ctx := domain.WithSessionID(context.Background(), "ses-e")
	prompt := strings.Repeat("p", 40)
	if _, err := p.GeneratorFor(domain.PurposeMemory).Generate(ctx, prompt); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	got := meter.Session("ses-e")
	if got.EstimatedCalls != 1 || got.InputTokens != EstimateTokens(prompt) || got.OutputTokens != 4 {
		t.Fatalf("fallback spend = %+v, want estimated 10/4", got)
	}
}

// ADR-0131 D3: once the session total plus the next prompt would pass the
// ceiling, the call is refused with the typed error BEFORE the provider is hit,
// and the session stays refused.
func TestSpendMeter_CeilingRefusesBeforeTheProvider(t *testing.T) {
	t.Parallel()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"x"}}],` +
			`"usage":{"prompt_tokens":80,"completion_tokens":40,"total_tokens":120}}`))
	}))
	defer srv.Close()

	p := httpProvider(t, srv.URL, "openai", 0, 0)
	meter := NewSpendMeter(100, nil, nil)
	p.SetSpendMeter(meter)
	gen := p.GeneratorFor(domain.PurposePlanner)
	ctx := domain.WithSessionID(context.Background(), "ses-c")

	if _, err := gen.Generate(ctx, "first"); err != nil {
		t.Fatalf("first call is under the ceiling: %v", err)
	}
	_, err := gen.Generate(ctx, "second")
	if !errors.Is(err, ErrSpendCeiling) {
		t.Fatalf("second call: want ErrSpendCeiling, got %v", err)
	}
	var ce *SpendCeilingError
	if !errors.As(err, &ce) || ce.SessionID != "ses-c" || ce.Used != 120 || ce.Ceiling != 100 || ce.Purpose != "planner" {
		t.Fatalf("typed error = %+v", ce)
	}
	if hits != 1 {
		t.Fatalf("a refused call must not reach the provider: %d hits", hits)
	}
	if !meter.Session("ses-c").Exhausted {
		t.Fatal("session must be marked exhausted after a refusal")
	}
	// Another session is unaffected; a call with no session is never refused.
	if _, err := gen.Generate(domain.WithSessionID(context.Background(), "ses-other"), "x"); err != nil {
		t.Fatalf("other session refused: %v", err)
	}
	if _, err := gen.Generate(context.Background(), "x"); err != nil {
		t.Fatalf("no-session call refused: %v", err)
	}
	if u := meter.NoSession(); u.Calls != 1 {
		t.Fatalf("no-session bucket = %+v, want 1 call", u)
	}
}

// Ceiling 0 meters but never refuses — the DefaultConfig posture.
func TestSpendMeter_ZeroCeilingNeverRefuses(t *testing.T) {
	t.Parallel()
	m := NewSpendMeter(0, nil, nil)
	m.RecordSession("s", "planner", "g", TokenUsage{PromptTokens: 1 << 20, CompletionTokens: 1 << 20})
	if err := m.AdmitSession("s", "planner", 1<<20); err != nil {
		t.Fatalf("ceiling 0 refused: %v", err)
	}
	var nilMeter *SpendMeter
	if err := nilMeter.Admit(context.Background(), "x", 1); err != nil {
		t.Fatalf("nil meter refused: %v", err)
	}
	nilMeter.Record(context.Background(), "x", "g", TokenUsage{TotalTokens: 1})
}

// The streaming surface books the usage the provider put on its trailing chunk.
func TestSpendMeter_StreamBooksTrailingUsage(t *testing.T) {
	t.Parallel()
	inner := &chunkStreamer{chunks: []domain.StreamChunk{
		{Text: "hel"}, {Text: "lo", IsFinal: true},
		{IsFinal: true, UsagePromptTokens: 7, UsageCompletionTokens: 2, UsageTotalTokens: 9, UsageCacheReadTokens: 5},
	}}
	m := NewSpendMeter(0, nil, nil)
	g := &meteredGenerator{inner: inner, meter: m, purpose: "memory", generatorID: "g"}
	ctx := domain.WithSessionID(context.Background(), "ses-s")
	ch, err := g.GenerateStream(ctx, "prompt")
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	for range ch {
	}
	got := m.Session("ses-s")
	if got.InputTokens != 7 || got.OutputTokens != 2 || got.CacheReadTokens != 5 || got.EstimatedCalls != 0 {
		t.Fatalf("stream spend = %+v", got)
	}
}

type chunkStreamer struct{ chunks []domain.StreamChunk }

func (c *chunkStreamer) Generate(context.Context, string) (string, error) { return "", nil }

func (c *chunkStreamer) GenerateStream(context.Context, string) (<-chan domain.StreamChunk, error) {
	ch := make(chan domain.StreamChunk, len(c.chunks))
	for _, x := range c.chunks {
		ch <- x
	}
	close(ch)
	return ch, nil
}

func httpProvider(t *testing.T, url, provider string, in, out float64) *Provider {
	t.Helper()
	p, err := NewProvider(config.LLMProviderConfig{
		Default: "g",
		Generators: []config.GeneratorConfig{{
			ID: "g", Provider: provider, Model: "m", Endpoint: url, APIKeyEnv: "BATCHF_UNSET_KEY",
			CostPer1MInput: in, CostPer1MOutput: out, TimeoutMs: 5000,
		}},
		Health: config.HealthConfig{FailureThreshold: 3, CooldownMs: 1000},
	}, nil)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	return p
}
