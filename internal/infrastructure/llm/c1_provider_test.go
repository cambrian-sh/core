package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// Provider-layer C1 behaviour (ADR-0139 D1): schema validate-and-repair, the
// fallback policy, and capability forwarding through every decorator.

// scriptedRG is a RequestGenerator that answers from a script.
type scriptedRG struct {
	mu      sync.Mutex
	replies []domain.GenerateResponse
	reqs    []domain.GenerateRequest
}

func (s *scriptedRG) GenerateRequest(_ context.Context, req domain.GenerateRequest) (domain.GenerateResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, req)
	if len(s.replies) == 0 {
		return domain.GenerateResponse{Text: "default", StopReason: domain.StopEndTurn}, nil
	}
	r := s.replies[0]
	if len(s.replies) > 1 {
		s.replies = s.replies[1:]
	}
	return r, nil
}

func (s *scriptedRG) StreamRequest(ctx context.Context, req domain.GenerateRequest) (<-chan domain.StreamChunk, error) {
	resp, _ := s.GenerateRequest(ctx, req)
	ch := make(chan domain.StreamChunk, 2)
	ch <- domain.StreamChunk{Text: resp.Text}
	ch <- usageChunk(&resp)
	close(ch)
	return ch, nil
}

func (s *scriptedRG) Capabilities() domain.Capabilities {
	return domain.Capabilities{ResponseSchema: domain.LevelBestEffort, Tools: domain.LevelEnforced}
}

func (s *scriptedRG) Generate(ctx context.Context, prompt string) (string, error) {
	return shimGenerate(ctx, s, prompt)
}

func (s *scriptedRG) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reqs)
}

var objectSchema = &domain.ResponseSchema{
	Name:   "verdict",
	Schema: []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`),
}

func schemaReq() domain.GenerateRequest {
	return domain.GenerateRequest{Messages: []domain.ModelMessage{domain.UserMessage("judge")}, ResponseSchema: objectSchema}
}

func TestSchema_ValidReplyPassesWithoutRepair(t *testing.T) {
	t.Parallel()
	inner := &scriptedRG{replies: []domain.GenerateResponse{{Text: "```json\n{\"ok\":true}\n```"}}}
	resp, err := (&schemaGenerator{inner: inner}).GenerateRequest(context.Background(), schemaReq())
	if err != nil {
		t.Fatal(err)
	}
	if inner.calls() != 1 {
		t.Errorf("a valid reply must not be repaired (%d calls)", inner.calls())
	}
	if resp.Text != `{"ok":true}` {
		t.Errorf("a fenced reply is returned as clean JSON, got %q", resp.Text)
	}
}

func TestSchema_InvalidReplyRepairedOnce(t *testing.T) {
	t.Parallel()
	inner := &scriptedRG{replies: []domain.GenerateResponse{
		{Text: `{"ok":"yes"}`, Usage: domain.GenerateUsage{Input: 10, Output: 2}},
		{Text: `{"ok":true}`, Usage: domain.GenerateUsage{Input: 20, Output: 2}},
	}}
	resp, err := (&schemaGenerator{inner: inner}).GenerateRequest(context.Background(), schemaReq())
	if err != nil {
		t.Fatal(err)
	}
	if inner.calls() != 2 || resp.Text != `{"ok":true}` {
		t.Fatalf("calls=%d text=%q", inner.calls(), resp.Text)
	}
	repair := inner.reqs[1].Messages
	if len(repair) != 3 || repair[1].Role != domain.RoleAssistant || repair[1].Content != `{"ok":"yes"}` {
		t.Fatalf("the repair turn must carry the failed reply, got %+v", repair)
	}
	if repair[2].Role != domain.RoleUser || !strings.Contains(repair[2].Content, "did not conform") {
		t.Errorf("the repair message must carry the validation error, got %q", repair[2].Content)
	}
	if resp.Usage.Input != 30 || resp.Usage.Output != 4 {
		t.Errorf("usage must cover both calls, got %+v", resp.Usage)
	}
}

func TestSchema_SecondFailureIsTypedError(t *testing.T) {
	t.Parallel()
	inner := &scriptedRG{replies: []domain.GenerateResponse{{Text: "not json at all"}}}
	_, err := (&schemaGenerator{inner: inner}).GenerateRequest(context.Background(), schemaReq())
	if !errors.Is(err, domain.ErrSchemaValidation) {
		t.Fatalf("err = %v, want ErrSchemaValidation", err)
	}
	var se *SchemaError
	if !errors.As(err, &se) || se.Name != "verdict" {
		t.Errorf("want a *SchemaError naming the schema, got %v", err)
	}
	if inner.calls() != 2 {
		t.Errorf("exactly one repair attempt, got %d calls", inner.calls())
	}
}

func TestSchema_IgnoredSchemaIsNotValidated(t *testing.T) {
	t.Parallel()
	inner := &scriptedRG{replies: []domain.GenerateResponse{{Text: "prose", Ignored: []string{domain.FieldResponseSchema}}}}
	resp, err := (&schemaGenerator{inner: inner}).GenerateRequest(context.Background(), schemaReq())
	if err != nil || resp.Text != "prose" || inner.calls() != 1 {
		t.Fatalf("a model that never saw the schema is not held to it: %q %v (%d calls)", resp.Text, err, inner.calls())
	}
}

func TestSchema_StreamWithSchemaIsValidated(t *testing.T) {
	t.Parallel()
	inner := &scriptedRG{replies: []domain.GenerateResponse{{Text: `{"ok":1}`}, {Text: `{"ok":false}`}}}
	ch, err := (&schemaGenerator{inner: inner}).StreamRequest(context.Background(), schemaReq())
	if err != nil {
		t.Fatal(err)
	}
	text, final := drain(t, ch)
	if text != `{"ok":false}` || final == nil || final.Text != `{"ok":false}` {
		t.Errorf("stream = %q final=%+v", text, final)
	}
}

// ── Fallback (D1) ────────────────────────────────────────────────────────────

type countingServer struct {
	srv  *httptest.Server
	hits atomic.Int32
}

func respondServer(t *testing.T, status int, body string) *countingServer {
	t.Helper()
	cs := &countingServer{}
	cs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		cs.hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(cs.srv.Close)
	return cs
}

func okBody(content, finish string) string {
	return `{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"` +
		finish + `","message":{"role":"assistant","content":` + content + `}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
}

func twoGeneratorProvider(t *testing.T, a, b *countingServer) *Provider {
	t.Helper()
	p, err := NewProvider(config.LLMProviderConfig{
		Default: "a",
		Generators: []config.GeneratorConfig{
			{ID: "a", Provider: "openai", Model: "m", Endpoint: a.srv.URL, TimeoutMs: 5000},
			{ID: "b", Provider: "openai", Model: "m", Endpoint: b.srv.URL, TimeoutMs: 5000},
		},
		Roles:  map[string]string{"planner": "a"},
		Health: config.HealthConfig{FailureThreshold: 5, CooldownMs: 60000},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFallback_On429SwitchesProvider(t *testing.T) {
	t.Parallel()
	// A quota 429: sharedLLMTransport hands it back at once (no backoff ladder),
	// so the D1 fallback is what moves the call.
	a := respondServer(t, http.StatusTooManyRequests, `{"error":{"type":"insufficient_quota","message":"quota exceeded"}}`)
	b := respondServer(t, http.StatusOK, okBody(`"from b"`, "stop"))
	p := twoGeneratorProvider(t, a, b)

	out, err := p.GeneratorFor(domain.PurposePlanner).Generate(context.Background(), "plan")
	if err != nil || out != "from b" {
		t.Fatalf("Generate = %q, %v; want the fallback generator's answer", out, err)
	}
	if a.hits.Load() != 1 || b.hits.Load() != 1 {
		t.Errorf("hits a=%d b=%d, want 1/1", a.hits.Load(), b.hits.Load())
	}
}

func TestFallback_On401RetriesOnceThenSwitches(t *testing.T) {
	t.Parallel()
	a := respondServer(t, http.StatusUnauthorized, `{"error":{"message":"invalid key"}}`)
	b := respondServer(t, http.StatusOK, okBody(`"from b"`, "stop"))
	p := twoGeneratorProvider(t, a, b)

	resp, err := p.GeneratorFor(domain.PurposePlanner).(domain.RequestGenerator).
		GenerateRequest(context.Background(), domain.UserPrompt("plan"))
	if err != nil || resp.Text != "from b" {
		t.Fatalf("GenerateRequest = %+v, %v", resp, err)
	}
	if a.hits.Load() != 2 {
		t.Errorf("a 401 is retried once with a refreshed credential before falling back; a hits = %d", a.hits.Load())
	}
}

func TestFallback_NeverOnRefusalSchemaFailureOrBadRequest(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		status int
		body   string
		req    domain.GenerateRequest
		check  func(t *testing.T, resp domain.GenerateResponse, err error)
	}{
		"refusal": {
			status: http.StatusOK, body: okBody(`"I can't help with that"`, "content_filter"), req: domain.UserPrompt("x"),
			check: func(t *testing.T, resp domain.GenerateResponse, err error) {
				if err != nil || resp.StopReason != domain.StopRefusal {
					t.Errorf("a refusal is an answer: resp=%+v err=%v", resp, err)
				}
			},
		},
		"schema failure": {
			status: http.StatusOK, body: okBody(`"definitely not json"`, "stop"),
			req: domain.GenerateRequest{Messages: []domain.ModelMessage{domain.UserMessage("x")}, ResponseSchema: objectSchema},
			check: func(t *testing.T, _ domain.GenerateResponse, err error) {
				if !errors.Is(err, domain.ErrSchemaValidation) {
					t.Errorf("err = %v, want ErrSchemaValidation", err)
				}
			},
		},
		"bad request": {
			status: http.StatusBadRequest, body: `{"error":{"message":"bad"}}`, req: domain.UserPrompt("x"),
			check: func(t *testing.T, _ domain.GenerateResponse, err error) {
				var pe *ProviderError
				if !errors.As(err, &pe) || pe.Status != http.StatusBadRequest {
					t.Errorf("err = %v, want the 400 itself", err)
				}
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			a := respondServer(t, tc.status, tc.body)
			b := respondServer(t, http.StatusOK, okBody(`"from b"`, "stop"))
			p := twoGeneratorProvider(t, a, b)
			resp, err := p.GeneratorFor(domain.PurposePlanner).(domain.RequestGenerator).GenerateRequest(context.Background(), tc.req)
			tc.check(t, resp, err)
			if b.hits.Load() != 0 {
				t.Errorf("%s must never fall back to another provider (b hit %d times)", name, b.hits.Load())
			}
		})
	}
}

func TestFallback_SingleGeneratorReturnsTheProviderError(t *testing.T) {
	t.Parallel()
	a := respondServer(t, http.StatusTooManyRequests, `{"error":{"type":"insufficient_quota","message":"quota exceeded"}}`)
	p := httpProvider(t, a.srv.URL, "openai", 0, 0)
	_, err := p.GeneratorFor(domain.PurposePlanner).Generate(context.Background(), "x")
	var pe *ProviderError
	if !errors.As(err, &pe) || pe.Status != http.StatusTooManyRequests {
		t.Fatalf("with nothing to fall back to, the provider's own error surfaces: %v", err)
	}
}

// ── Forwarding through every decorator ───────────────────────────────────────

func decoratorsAround(inner domain.Generator) map[string]domain.Generator {
	meter := NewSpendMeter(0, SeedPriceLedger(nil), nil)
	return map[string]domain.Generator{
		"healthGenerator":      newHealthGenerator("g1", inner, NewCircuitBreaker(3, time.Minute)),
		"concurrencyGenerator": &concurrencyGenerator{inner: inner, sem: make(chan struct{}, 1)},
		"meteredGenerator":     &meteredGenerator{inner: inner, meter: meter, purpose: "p", generatorID: "g1"},
		"schemaGenerator":      &schemaGenerator{inner: inner},
		"traceBypass":          &traceBypass{traced: inner, untraced: inner},
	}
}

func TestDecorators_ForwardRequestGenerator(t *testing.T) {
	t.Parallel()
	for name, dec := range decoratorsAround(&scriptedRG{}) {
		rg, ok := domain.AsRequestGenerator(dec)
		if !ok {
			t.Fatalf("%s must forward the RequestGenerator capability", name)
		}
		resp, err := rg.GenerateRequest(context.Background(), domain.UserPrompt("go"))
		if err != nil || resp.Text != "default" {
			t.Errorf("%s: GenerateRequest = %+v, %v", name, resp, err)
		}
		ch, err := rg.StreamRequest(context.Background(), domain.UserPrompt("go"))
		if err != nil {
			t.Fatalf("%s: StreamRequest: %v", name, err)
		}
		if _, final := drain(t, ch); final == nil {
			t.Errorf("%s dropped the final response chunk", name)
		}
		if rg.Capabilities().ResponseSchema != domain.LevelBestEffort {
			t.Errorf("%s must forward the inner's Capabilities", name)
		}
	}
}

func TestDecorators_DoNotClaimRequestsForAPlainInner(t *testing.T) {
	t.Parallel()
	for name, dec := range decoratorsAround(plainGen{}) {
		if name == "traceBypass" {
			continue // only ever built around an inner that IS a RequestGenerator
		}
		if _, ok := domain.AsRequestGenerator(dec); ok {
			t.Errorf("%s over-reports RequestGenerator for a plain inner", name)
		}
	}
}

// A trace wrapper that predates C1 (the premium Langfuse wrapper exposes only
// Generate and GenerateStream) must neither hide the C1 path nor bypass tracing
// on the legacy path.
type generateOnlyTracer struct {
	inner domain.Generator
	calls *atomic.Int32
}

func (g generateOnlyTracer) Generate(ctx context.Context, prompt string) (string, error) {
	g.calls.Add(1)
	return g.inner.Generate(ctx, prompt)
}

func TestProvider_LegacyTraceWrapperDoesNotHideC1(t *testing.T) {
	t.Parallel()
	srv := respondServer(t, http.StatusOK, okBody(`"ok"`, "stop"))
	p := httpProvider(t, srv.srv.URL, "openai", 0, 0)
	var traced atomic.Int32
	p.SetTraceWrapper(func(g domain.Generator, _ string) domain.Generator {
		return generateOnlyTracer{inner: g, calls: &traced}
	})
	gen, err := p.Acquire(context.Background(), domain.LLMRequest{Purpose: domain.PurposePlanner})
	if err != nil {
		t.Fatal(err)
	}
	rg, ok := domain.AsRequestGenerator(gen)
	if !ok {
		t.Fatal("a legacy trace wrapper must not hide the C1 request path")
	}
	if _, err := rg.GenerateRequest(context.Background(), domain.UserPrompt("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := gen.Generate(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if traced.Load() != 1 {
		t.Errorf("the legacy Generate path must stay traced (traced=%d)", traced.Load())
	}
	if domain.SupportsToolCalling(gen) {
		t.Error("a wrapper that hides tool calling keeps hiding it (no behaviour change)")
	}
}

func TestShims_PreserveLegacyRequestShape(t *testing.T) {
	t.Parallel()
	inner := &scriptedRG{}
	if _, err := shimGenerate(context.Background(), inner, "p"); err != nil {
		t.Fatal(err)
	}
	if _, err := shimGenerateWithTools(context.Background(), inner,
		[]domain.ModelMessage{domain.UserMessage("q")}, []domain.ToolDefinition{{Name: "t"}}); err != nil {
		t.Fatal(err)
	}
	g, w := inner.reqs[0], inner.reqs[1]
	if len(g.System) != 0 || len(g.Messages) != 1 || g.Messages[0].Content != "p" || g.Messages[0].Role != domain.RoleUser ||
		g.ResponseSchema != nil || g.Options.MaxTokens != 0 || g.Options.Temperature != nil || len(g.Tools) != 0 {
		t.Errorf("Generate must shim to exactly one user turn and nothing else, got %+v", g)
	}
	if len(w.Tools) != 1 || w.ToolChoice != (domain.ToolChoice{}) {
		t.Errorf("GenerateWithTools must shim to tools + auto choice, got %+v", w)
	}
}
