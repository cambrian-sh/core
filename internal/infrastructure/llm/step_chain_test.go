package llm

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0139 D1, wave 1c: the agent-plane gateway serves a step's model chain through
// Provider.GenerateOn / StreamOn — the same fallback policy, breaker and decorator
// stack the organs use, and NOT the spend meter (the gateway books lease calls).

func TestGenerateOn_FallsDownTheChainOn429(t *testing.T) {
	t.Parallel()
	a := respondServer(t, http.StatusTooManyRequests, `{"error":{"type":"insufficient_quota","message":"quota exceeded"}}`)
	b := respondServer(t, http.StatusOK, okBody(`"from b"`, "stop"))
	p := twoGeneratorProvider(t, a, b)

	resp, served, err := p.GenerateOn(context.Background(), []string{"a", "b"}, domain.UserPrompt("x"))
	if err != nil || resp.Text != "from b" || served != "b" {
		t.Fatalf("GenerateOn = %q served by %q, %v; want b's answer", resp.Text, served, err)
	}
	if b.hits.Load() != 1 {
		t.Errorf("b hits = %d, want 1", b.hits.Load())
	}
}

func TestGenerateOn_NeverFallsBackOnABadRequest(t *testing.T) {
	t.Parallel()
	a := respondServer(t, http.StatusBadRequest, `{"error":{"message":"bad"}}`)
	b := respondServer(t, http.StatusOK, okBody(`"from b"`, "stop"))
	p := twoGeneratorProvider(t, a, b)

	_, served, err := p.GenerateOn(context.Background(), []string{"a", "b"}, domain.UserPrompt("x"))
	var pe *ProviderError
	if !errors.As(err, &pe) || pe.Status != http.StatusBadRequest || served != "a" {
		t.Fatalf("err = %v served = %q; want a's own 400", err, served)
	}
	if b.hits.Load() != 0 {
		t.Errorf("a 4xx must not move down the chain (b hit %d times)", b.hits.Load())
	}
}

func TestGenerateOn_UnsupportedToolsIsRefusedWithoutAWireCall(t *testing.T) {
	t.Parallel()
	a := respondServer(t, http.StatusOK, okBody(`"from a"`, "stop"))
	b := respondServer(t, http.StatusOK, okBody(`"from b"`, "stop"))
	p := twoGeneratorProvider(t, a, b) // neither generator declares native_tools

	req := domain.UserPrompt("x")
	req.Tools = []domain.ToolDefinition{{Name: "t", Parameters: []byte(`{"type":"object"}`)}}
	_, _, err := p.GenerateOn(context.Background(), []string{"a", "b"}, req)
	if !errors.Is(err, domain.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	if a.hits.Load()+b.hits.Load() != 0 {
		t.Errorf("an unsupported field is refused before the wire (hits a=%d b=%d)", a.hits.Load(), b.hits.Load())
	}
}

func TestGenerateOn_SkipsUnknownCandidatesAndReportsAnEmptyChain(t *testing.T) {
	t.Parallel()
	a := respondServer(t, http.StatusOK, okBody(`"from a"`, "stop"))
	b := respondServer(t, http.StatusOK, okBody(`"from b"`, "stop"))
	p := twoGeneratorProvider(t, a, b)

	resp, served, err := p.GenerateOn(context.Background(), []string{"removed", "b"}, domain.UserPrompt("x"))
	if err != nil || served != "b" || resp.Text != "from b" {
		t.Fatalf("GenerateOn = %q by %q, %v; want the first KNOWN candidate", resp.Text, served, err)
	}
	if _, _, err := p.GenerateOn(context.Background(), []string{"removed"}, domain.UserPrompt("x")); !errors.Is(err, ErrNoHealthyModel) {
		t.Fatalf("no servable candidate: err = %v, want ErrNoHealthyModel", err)
	}
}

func TestGenerateOn_IsNotMeteredByTheProvider(t *testing.T) {
	t.Parallel()
	a := respondServer(t, http.StatusOK, okBody(`"from a"`, "stop"))
	b := respondServer(t, http.StatusOK, okBody(`"from b"`, "stop"))
	p := twoGeneratorProvider(t, a, b)
	meter := NewSpendMeter(0, nil, nil)
	p.SetSpendMeter(meter)

	ctx := domain.WithSessionID(context.Background(), "s1")
	if _, _, err := p.GenerateOn(ctx, []string{"a"}, domain.UserPrompt("x")); err != nil {
		t.Fatal(err)
	}
	if got := meter.Session("s1"); got.Calls != 0 || got.InputTokens != 0 {
		t.Fatalf("the Provider booked a gateway call (%+v); the gateway meters lease calls itself", got)
	}
	// The organ path through the same Provider IS metered — the contrast that
	// proves the test can see a booking.
	if _, err := p.GeneratorFor(domain.PurposePlanner).Generate(ctx, "plan"); err != nil {
		t.Fatal(err)
	}
	if got := meter.Session("s1"); got.Calls != 1 {
		t.Fatalf("organ call not booked: %+v", got)
	}
}

func TestStreamOn_ServesTheStreamThroughTheStack(t *testing.T) {
	t.Parallel()
	a := respondServer(t, http.StatusTooManyRequests, `{"error":{"type":"insufficient_quota","message":"quota exceeded"}}`)
	b := respondServer(t, http.StatusOK, okBody(`"\"from b\""`, "stop"))
	p := twoGeneratorProvider(t, a, b)

	// A schema-bearing stream is served as a validated GenerateRequest (the schema
	// layer), so this also proves the stream crosses the whole stack.
	req := domain.UserPrompt("x")
	req.ResponseSchema = &domain.ResponseSchema{Name: "s", Schema: []byte(`{"type":"string"}`)}
	ch, served, err := p.StreamOn(context.Background(), []string{"a", "b"}, req)
	if err != nil || served != "b" {
		t.Fatalf("StreamOn served by %q, %v", served, err)
	}
	var final *domain.GenerateResponse
	for c := range ch {
		if c.Response != nil {
			final = c.Response
		}
	}
	if final == nil || final.Text != `"from b"` {
		t.Fatalf("final response = %+v", final)
	}
}
