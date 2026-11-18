package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// T-20261112-pre-send-context-window-check-per-model-with.

// b64Pattern is a base64-looking chunk (mixed case and digits). The SDK's
// test_context_engine.py builds the same vectors from the same pattern, so the
// kernel and the SDK estimate a prompt identically.
const b64Pattern = "QUJDZGVmMTIz"

func b64Run(n int) string { return strings.Repeat(b64Pattern, n/len(b64Pattern)+1)[:n] }

func TestEstimateTokens_ContentAware(t *testing.T) {
	t.Parallel()
	prose := strings.Repeat("abc ", 100) // 400 chars of ordinary text
	cases := []struct {
		name string
		text string
		want int
	}{
		{"empty", "", 0},
		{"prose at 4 chars per token", prose, 100},
		{"base64 at 1.3 chars per token", b64Run(1300), 1000},
		{"prose then base64", prose + b64Run(1300), 1100},
		{"sha256 hex (lower + digits)", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", 50},
		// One character class is not high entropy: a long lower-case run stays 4:1.
		{"single class run", strings.Repeat("a", 100), 25},
		// Below the minimum run length the 4:1 rule applies.
		{"short mixed run", b64Run(config.HighEntropyMinRun - 4), (config.HighEntropyMinRun - 4) / 4},
		{"tiny text is at least one token", "hi", 1},
	}
	for _, c := range cases {
		if got := EstimateTokens(c.text); got != c.want {
			t.Errorf("%s: EstimateTokens = %d, want %d", c.name, got, c.want)
		}
	}
}

// The p06 shape: a prompt that is mostly base64. len/4 estimated it ~2.8x under
// what the provider counted; the content-aware estimate must not under-count it.
func TestEstimateTokens_P06ShapeIsNotUnderCounted(t *testing.T) {
	t.Parallel()
	text := "Tool result:\n" + b64Run(400000)
	providerCounted := 400000 * 100 / 144 // ~1.44 chars/token, what p06 implies
	if got := EstimateTokens(text); got < providerCounted {
		t.Fatalf("estimate %d under-counts the provider's ~%d", got, providerCounted)
	}
}

// countingGen records whether a call reached it on every surface.
type countingGen struct{ calls atomic.Int32 }

func (c *countingGen) Generate(context.Context, string) (string, error) {
	c.calls.Add(1)
	return "ok", nil
}
func (c *countingGen) GenerateWithTools(context.Context, []domain.ModelMessage, []domain.ToolDefinition) (domain.ModelTurn, error) {
	c.calls.Add(1)
	return domain.ModelTurn{Text: "ok"}, nil
}
func (c *countingGen) GenerateStream(context.Context, string) (<-chan domain.StreamChunk, error) {
	c.calls.Add(1)
	ch := make(chan domain.StreamChunk, 1)
	ch <- domain.StreamChunk{Text: "ok", IsFinal: true}
	close(ch)
	return ch, nil
}
func (c *countingGen) GenerateRequest(context.Context, domain.GenerateRequest) (domain.GenerateResponse, error) {
	c.calls.Add(1)
	return domain.GenerateResponse{Text: "ok"}, nil
}
func (c *countingGen) StreamRequest(context.Context, domain.GenerateRequest) (<-chan domain.StreamChunk, error) {
	return c.GenerateStream(context.Background(), "")
}
func (c *countingGen) Capabilities() domain.Capabilities { return domain.Capabilities{} }

func TestContextGuard_RefusesOverWindowWithoutCallingInner(t *testing.T) {
	t.Parallel()
	// window 1000, output cap 100, margin 5% (50) => prompt budget 850 tokens.
	inner := &countingGen{}
	g := &contextGuardGenerator{inner: inner, id: "m", window: 1000, maxTokens: 100, margin: 0.05}
	over := strings.Repeat("abcd", 851) // 851 tokens
	fits := strings.Repeat("abcd", 850) // 850 tokens

	ctx := context.Background()
	calls := map[string]func(string) error{
		"Generate": func(p string) error { _, err := g.Generate(ctx, p); return err },
		"GenerateWithTools": func(p string) error {
			_, err := g.GenerateWithTools(ctx, []domain.ModelMessage{domain.UserMessage(p)}, nil)
			return err
		},
		"GenerateStream":  func(p string) error { _, err := g.GenerateStream(ctx, p); return err },
		"GenerateRequest": func(p string) error { _, err := g.GenerateRequest(ctx, domain.UserPrompt(p)); return err },
		"StreamRequest":   func(p string) error { _, err := g.StreamRequest(ctx, domain.UserPrompt(p)); return err },
	}
	for name, call := range calls {
		before := inner.calls.Load()
		if err := call(over); !errors.Is(err, domain.ErrContextOverflow) {
			t.Fatalf("%s over the window: err = %v, want ErrContextOverflow", name, err)
		}
		if inner.calls.Load() != before {
			t.Fatalf("%s: an over-window prompt reached the provider", name)
		}
		if err := call(fits); err != nil {
			t.Fatalf("%s at the budget: err = %v, want the call to go through", name, err)
		}
		if inner.calls.Load() != before+1 {
			t.Fatalf("%s: a prompt that fits did not reach the provider", name)
		}
	}
}

// The request's own max_tokens is the output reservation when it sets one.
func TestContextGuard_RequestMaxTokensOverridesTheGeneratorCap(t *testing.T) {
	t.Parallel()
	inner := &countingGen{}
	g := &contextGuardGenerator{inner: inner, id: "m", window: 1000, maxTokens: 100, margin: 0.05}
	req := domain.UserPrompt(strings.Repeat("abcd", 800)) // 800 tokens
	req.Options.MaxTokens = 200                           // 800 + 200 + 50 > 1000
	if _, err := g.GenerateRequest(context.Background(), req); !errors.Is(err, domain.ErrContextOverflow) {
		t.Fatalf("err = %v, want ErrContextOverflow (request max_tokens reserved)", err)
	}
	req.Options.MaxTokens = 0 // falls back to the generator's 100: 800 + 100 + 50 fits
	if _, err := g.GenerateRequest(context.Background(), req); err != nil {
		t.Fatalf("err = %v, want the call to go through", err)
	}
}

// Through the real Provider over a real OpenAI-compatible client: the guard is
// installed for a generator with a window (and only then), its refusal is a
// request fault (no breaker event), and no HTTP request is made.
func TestProvider_PreSendCheckSpendsNoCall(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer srv.Close()

	cfg := config.LLMProviderConfig{
		Default: "small",
		Generators: []config.GeneratorConfig{
			{ID: "small", Provider: "openai", Model: "m", Endpoint: srv.URL, TimeoutMs: 5000, MaxTokens: 100, ContextWindowTokens: 1000},
			{ID: "unknown", Provider: "openai", Model: "m", Endpoint: srv.URL, TimeoutMs: 5000},
		},
		Roles:  map[string]string{"planner": "small", "verifier": "unknown"},
		Health: config.HealthConfig{FailureThreshold: 1, CooldownMs: 60000},
	}
	p, err := NewProvider(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	gen, err := p.Acquire(ctx, domain.LLMRequest{Purpose: domain.PurposePlanner})
	if err != nil {
		t.Fatal(err)
	}
	over := b64Run(1300) // 1000 tokens content-aware; len/4 would have said 325 and let it through
	if _, err := gen.Generate(ctx, over); !errors.Is(err, domain.ErrContextOverflow) {
		t.Fatalf("err = %v, want ErrContextOverflow", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("the provider saw %d requests for an over-window prompt", hits.Load())
	}
	// Breaker threshold 1: had the refusal counted against health, this would fail
	// with no healthy model instead of reaching the endpoint.
	if out, err := gen.Generate(ctx, "say hello"); err != nil || out != "hello" {
		t.Fatalf("near-window call = %q, %v; want the provider's reply", out, err)
	}
	if hits.Load() != 1 {
		t.Fatalf("provider saw %d requests, want 1", hits.Load())
	}
	// A generator with no window (0 = unknown) has no pre-send check.
	unk, err := p.Acquire(ctx, domain.LLMRequest{Purpose: domain.PurposeVerifier})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unk.Generate(ctx, over); err != nil {
		t.Fatalf("a generator without a window refused: %v", err)
	}
	if hits.Load() != 2 {
		t.Fatalf("provider saw %d requests, want 2", hits.Load())
	}
}
