package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// T-20261112-worker-resolve-cid-inline-has-no-size-cap-a: a prompt over the
// model's context window is a property of the REQUEST. It must be recognisable
// (domain.ErrContextOverflow), never trigger fallback, and never count against the
// model's health.

func TestWrapProviderError_PromptTooLong(t *testing.T) {
	t.Parallel()
	// A provider's 400 request error through a real SDK client (status + body).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"prompt is too long: 205074 tokens > 200000 maximum"}}`)
	}))
	defer srv.Close()
	c := &OpenAIClient{Endpoint: srv.URL, Model: "m"}
	_, err := c.Generate(context.Background(), "hi")
	var pe *ProviderError
	if !errors.As(err, &pe) || pe.Status != http.StatusBadRequest {
		t.Fatalf("err = %v, want a ProviderError", err)
	}
	if !errors.Is(err, domain.ErrContextOverflow) {
		t.Fatalf("400 'prompt is too long' is not ErrContextOverflow: %v", err)
	}

	cases := map[string]struct {
		err  error
		want bool
	}{
		// The claudebox proxy streams the refusal after a 200 as a plain error.
		"streamed server_error": {
			err:  errors.New(`received error while streaming: {"message":"Prompt is too long · the request is ~205074 tokens (limit 200000)","type":"server_error"}`),
			want: true,
		},
		"unrelated transport error": {err: errors.New("connection reset by peer"), want: false},
	}
	for name, c := range cases {
		got := errors.Is(wrapProviderError("openai", c.err), domain.ErrContextOverflow)
		if got != c.want {
			t.Errorf("%s: errors.Is(ErrContextOverflow) = %v, want %v", name, got, c.want)
		}
	}
}

func TestIsContextOverflowRefusal_StatusAndCodeFirst(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"anthropic 400", 400, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 205074 tokens > 200000 maximum"}}`, true},
		{"openai code", 400, `{"error":{"message":"This model's maximum context length is 128000 tokens.","type":"invalid_request_error","code":"context_length_exceeded"}}`, true},
		{"openai code on another status", 422, `{"error":{"code":"context_length_exceeded"}}`, true},
		{"gemini 400", 400, `The input token count (1200000) exceeds the maximum number of tokens allowed (1048576).`, true},
		{"413", 413, `prompt is too long`, true},
		{"plain 400", 400, `{"error":{"type":"invalid_request_error","message":"tools.0.name: String should match pattern"}}`, false},
		// A 500 is only an overflow when a structured code says so: the text match
		// is reserved for request errors and status-less streamed errors.
		{"500 text only", 500, `prompt is too long`, false},
	}
	for _, c := range cases {
		pe := providerError("p", c.status, c.body, errors.New("x"))
		if got := errors.Is(pe, domain.ErrContextOverflow); got != c.want {
			t.Errorf("%s: overflow = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestClassifyForFallback_ContextOverflowNoFallback(t *testing.T) {
	ctx := context.Background()
	overflow400 := providerError("anthropic", 400, `{"error":{"type":"invalid_request_error","message":"prompt is too long"}}`, errors.New("x"))
	streamed := wrapProviderError("openai", errors.New(`{"message":"Prompt is too long","type":"server_error"}`))
	for name, err := range map[string]error{"400": overflow400, "streamed": streamed} {
		if got := classifyForFallback(ctx, err); got != noFallback {
			t.Errorf("%s: classifyForFallback = %v, want noFallback", name, got)
		}
	}
	// A genuine streamed outage still falls back.
	if got := classifyForFallback(ctx, wrapProviderError("openai", errors.New("unexpected EOF"))); got != fallbackNow {
		t.Errorf("transport failure: got %v, want fallbackNow", got)
	}
}

// overflowGen is a RequestGenerator that always refuses with an overflow.
type overflowGen struct{}

func (overflowGen) Generate(context.Context, string) (string, error) {
	return "", domain.ErrContextOverflow
}
func (g overflowGen) GenerateRequest(context.Context, domain.GenerateRequest) (domain.GenerateResponse, error) {
	return domain.GenerateResponse{}, wrapProviderError("p", errors.New("Prompt is too long"))
}
func (g overflowGen) StreamRequest(context.Context, domain.GenerateRequest) (<-chan domain.StreamChunk, error) {
	ch := make(chan domain.StreamChunk, 1)
	streamFinal(ch, nil, wrapProviderError("p", errors.New("Prompt is too long")))
	close(ch)
	return ch, nil
}
func (overflowGen) Capabilities() domain.Capabilities { return domain.Capabilities{} }

func TestHealthGenerator_ContextOverflowIsNotAHealthEvent(t *testing.T) {
	br := NewCircuitBreaker(1, 0)
	h := newHealthGenerator("m", overflowGen{}, br)
	rg, ok := domain.AsRequestGenerator(h)
	if !ok {
		t.Fatal("health wrapper lost the request path")
	}
	for range 3 {
		if _, err := rg.GenerateRequest(context.Background(), domain.GenerateRequest{}); !errors.Is(err, domain.ErrContextOverflow) {
			t.Fatalf("GenerateRequest err = %v, want ErrContextOverflow", err)
		}
		ch, err := rg.StreamRequest(context.Background(), domain.GenerateRequest{})
		if err != nil {
			t.Fatal(err)
		}
		var last domain.StreamChunk
		for c := range ch {
			last = c
		}
		if !errors.Is(last.Err, domain.ErrContextOverflow) {
			t.Fatalf("final chunk Err = %v, want ErrContextOverflow", last.Err)
		}
	}
	if !br.Healthy("m") {
		t.Fatal("a context overflow opened the breaker: it is the request's fault, not the model's")
	}
}
