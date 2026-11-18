package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// T-20261112-a-provider-auth-failure-streamed-through-the (owner decision
// 2026-11-18): a refused provider credential is domain.ErrProviderAuth — detected
// from the status and the error envelope first, the message text only where a
// proxy left no status — and it is never retried, never falls back and never
// counts against the model's health.

func TestIsAuthRefusal_StatusAndTypeFirst(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"401 any body", 401, `{"error":{"message":"invalid key"}}`, true},
		{"403", 403, `forbidden`, true},
		{"anthropic envelope", 400, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, true},
		{"permission_error envelope", 400, `{"error":{"type":"permission_error","message":"no access"}}`, true},
		{"openai code", 400, `{"error":{"type":"invalid_request_error","code":"invalid_api_key","message":"Incorrect API key"}}`, true},
		{"proxy relays it as a 500", 500, `{"error":{"type":"server_error","message":"Failed to authenticate: OAuth token revoked"}}`, true},
		{"plain 500", 500, `{"error":{"type":"api_error","message":"Internal server error"}}`, false},
		{"429", 429, `{"error":{"type":"rate_limit_error","message":"Rate limited"}}`, false},
		// Text alone never classifies a 4xx request error: status and type decide.
		{"400 text only", 400, `the tool said: failed to authenticate`, false},
	}
	for _, c := range cases {
		pe := providerError("p", c.status, c.body, errors.New("x"))
		if got := errors.Is(pe, domain.ErrProviderAuth); got != c.want {
			t.Errorf("%s: auth = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestWrapProviderError_StatuslessClasses(t *testing.T) {
	t.Parallel()
	streamed := wrapProviderError("openai", errors.New(`received error while streaming: {"message":"Failed to authenticate: OAuth token revoked","type":"server_error"}`))
	if !errors.Is(streamed, domain.ErrProviderAuth) || errors.Is(streamed, ErrProviderUnreachable) {
		t.Fatalf("streamed auth failure: %v, want ErrProviderAuth only", streamed)
	}
	reset := wrapProviderError("openai", errors.New("connection reset by peer"))
	if !errors.Is(reset, ErrProviderUnreachable) || errors.Is(reset, domain.ErrProviderAuth) {
		t.Fatalf("reset: %v, want ErrProviderUnreachable", reset)
	}
	if reset.Error() != "openai: connection reset by peer" {
		t.Errorf("an unreachable endpoint's message changed: %q", reset.Error())
	}
	cancelled := wrapProviderError("openai", context.Canceled)
	if errors.Is(cancelled, ErrProviderUnreachable) || !errors.Is(cancelled, context.Canceled) {
		t.Fatalf("the caller's cancellation is not an endpoint failure: %v", cancelled)
	}
}

func TestWrapProviderError_401ThroughRealClientAndRetryAfter(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"type":"authentication_error","message":"OAuth token revoked","code":"authentication_error"}}`)
	}))
	defer srv.Close()
	_, err := (&OpenAIClient{Endpoint: srv.URL, Model: "m"}).Generate(context.Background(), "hi")
	var pe *ProviderError
	if !errors.As(err, &pe) || pe.Status != http.StatusUnauthorized || !errors.Is(err, domain.ErrProviderAuth) {
		t.Fatalf("err = %v, want a 401 ProviderError that is ErrProviderAuth", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("a 401 was retried: %d requests", hits.Load())
	}

	mk := func(v string) *http.Response {
		return &http.Response{Header: http.Header{"Retry-After": []string{v}}}
	}
	if d := retryAfterOf(mk("20")); d != 20*time.Second {
		t.Errorf("Retry-After 20 = %v", d)
	}
	if d := retryAfterOf(mk(time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat))); d < 25*time.Second || d > 31*time.Second {
		t.Errorf("Retry-After date = %v, want ~30s", d)
	}
	if d := retryAfterOf(mk("soon")); d != 0 {
		t.Errorf("unreadable Retry-After = %v, want 0", d)
	}
	if d := retryAfterOf(nil); d != 0 {
		t.Errorf("no response = %v", d)
	}
}

func TestClassifyForFallback_AuthNeverFallsBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, err := range map[string]error{
		"401":      providerError("openai", 401, `{}`, errors.New("x")),
		"403":      providerError("anthropic", 403, `{}`, errors.New("x")),
		"streamed": wrapProviderError("openai", errors.New("Failed to authenticate: OAuth token revoked")),
	} {
		if got := classifyForFallback(ctx, err); got != noFallback {
			t.Errorf("%s: classifyForFallback = %v, want noFallback", name, got)
		}
	}
	for name, err := range map[string]error{
		"429":   providerError("openai", 429, `{}`, errors.New("x")),
		"500":   providerError("openai", 500, `{}`, errors.New("x")),
		"reset": wrapProviderError("openai", errors.New("connection reset by peer")),
	} {
		if got := classifyForFallback(ctx, err); got != fallbackNow {
			t.Errorf("%s: classifyForFallback = %v, want fallbackNow", name, got)
		}
	}
}

// authGen refuses every call with a streamed credential refusal.
type authGen struct{}

func (authGen) Generate(context.Context, string) (string, error) {
	return "", wrapProviderError("p", errors.New("Failed to authenticate: OAuth token revoked"))
}
func (authGen) GenerateRequest(context.Context, domain.GenerateRequest) (domain.GenerateResponse, error) {
	return domain.GenerateResponse{}, wrapProviderError("p", errors.New("Failed to authenticate: OAuth token revoked"))
}
func (authGen) StreamRequest(context.Context, domain.GenerateRequest) (<-chan domain.StreamChunk, error) {
	ch := make(chan domain.StreamChunk, 1)
	streamFinal(ch, nil, wrapProviderError("p", errors.New("Failed to authenticate: OAuth token revoked")))
	close(ch)
	return ch, nil
}
func (authGen) Capabilities() domain.Capabilities { return domain.Capabilities{} }

func TestHealthGenerator_AuthFailureIsNotAHealthEvent(t *testing.T) {
	t.Parallel()
	br := NewCircuitBreaker(1, 0)
	h := newHealthGenerator("m", authGen{}, br)
	rg, _ := domain.AsRequestGenerator(h)
	for range 3 {
		if _, err := h.Generate(context.Background(), "x"); !errors.Is(err, domain.ErrProviderAuth) {
			t.Fatalf("Generate err = %v", err)
		}
		if _, err := rg.GenerateRequest(context.Background(), domain.GenerateRequest{}); !errors.Is(err, domain.ErrProviderAuth) {
			t.Fatalf("GenerateRequest err = %v", err)
		}
		ch, err := rg.StreamRequest(context.Background(), domain.GenerateRequest{})
		if err != nil {
			t.Fatal(err)
		}
		for range ch {
		}
	}
	if !br.Healthy("m") {
		t.Fatal("a refused credential opened the breaker")
	}
}

// A proxy relaying the credential refusal as a 500 is not replayed by the
// transport's 5xx backoff.
func TestTransport_RelayedAuth500IsNotRetried(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"type":"server_error","message":"Failed to authenticate: OAuth token revoked"}}`)
	}))
	defer srv.Close()
	_, err := (&OpenAIClient{Endpoint: srv.URL, Model: "m"}).Generate(context.Background(), "hi")
	if !errors.Is(err, domain.ErrProviderAuth) {
		t.Fatalf("err = %v, want ErrProviderAuth", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("the relayed refusal was retried: %d requests", hits.Load())
	}
}
