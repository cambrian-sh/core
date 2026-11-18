package network

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/stepoutput"
)

// T-20261112-a-provider-auth-failure-streamed-through-the (owner decision
// 2026-11-18): every provider failure class reached the agent as Internal. A
// refused credential is Unauthenticated (no retry, no fallback, no breaker event),
// a rate limit that outlived the transport's backoff is ResourceExhausted with the
// provider's Retry-After, and a 5xx or an endpoint that never answered is
// Unavailable.

func TestGatewayStatus_ProviderFaultClasses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want codes.Code
	}{
		{"401", &llm.ProviderError{Provider: "openai", Status: 401, Message: "x", Auth: true}, codes.Unauthenticated},
		{"streamed auth", fmt.Errorf("openai: %w: received error while streaming: Failed to authenticate: OAuth token revoked", domain.ErrProviderAuth), codes.Unauthenticated},
		{"429", &llm.ProviderError{Provider: "openai", Status: 429, Message: "rate"}, codes.ResourceExhausted},
		{"500", &llm.ProviderError{Provider: "openai", Status: 500, Message: "boom"}, codes.Unavailable},
		{"503", &llm.ProviderError{Provider: "openai", Status: 503, Message: "busy"}, codes.Unavailable},
		{"reset", fmt.Errorf("openai: %w: connection reset by peer", llm.ErrProviderUnreachable), codes.Unavailable},
		// A plain 400 request error is not a provider-outage class: unchanged.
		{"400", &llm.ProviderError{Provider: "openai", Status: 400, Message: "bad"}, codes.Internal},
		// A context overflow keeps OutOfRange even on a 400.
		{"overflow", &llm.ProviderError{Provider: "openai", Status: 400, Message: "x", Overflow: true}, codes.OutOfRange},
	}
	for _, c := range cases {
		if got := status.Code(gatewayStatus(c.err)); got != c.want {
			t.Errorf("%s: code = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestGatewayStatus_RateLimitCarriesRetryAfter(t *testing.T) {
	t.Parallel()
	err := gatewayStatus(&llm.ProviderError{Provider: "openai", Status: 429, Message: "rate", RetryAfter: 20 * time.Second})
	if got := retryDelay(err); got != 20*time.Second {
		t.Fatalf("RetryInfo delay = %v, want 20s (status %v)", got, status.Convert(err).Proto())
	}
	if d := retryDelay(gatewayStatus(&llm.ProviderError{Provider: "openai", Status: 429, Message: "rate"})); d != 0 {
		t.Fatalf("no Retry-After must mean no RetryInfo, got %v", d)
	}
}

func retryDelay(err error) time.Duration {
	for _, d := range status.Convert(err).Details() {
		if ri, ok := d.(*errdetails.RetryInfo); ok {
			return ri.GetRetryDelay().AsDuration()
		}
	}
	return 0
}

// ── End to end: the real gateway over the real llm.Provider ─────────────────

// The fault shapes are the readiness run's fault proxy (ENG-1995 phase B block 7,
// faultproxy.py): an OpenAI-compatible error envelope per status, the claudebox
// proxy's streamed auth failure after a 200, and a connection closed unanswered.
const (
	faultAuth       = "auth"
	faultAuthStream = "auth_stream"
	fault429        = "429"
	fault500        = "500"
	faultReset      = "reset"
)

const okSSE = `data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}

data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`

// faultServer answers every request with fault ("" = a good streamed reply) and
// counts the requests it saw.
func faultServer(t *testing.T, fault string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	envelope := func(w http.ResponseWriter, code int, etype, msg string, hdr map[string]string) {
		for k, v := range hdr {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = fmt.Fprintf(w, `{"error":{"type":%q,"message":%q,"code":%q}}`, etype, msg, etype)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		switch fault {
		case faultAuth:
			envelope(w, http.StatusUnauthorized, "authentication_error", "OAuth token revoked", nil)
		case faultAuthStream:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"error":{"message":"Failed to authenticate: OAuth token revoked","type":"server_error"}}`+"\n\n")
		case fault429:
			// Retry-After 0 lets the transport's five backoff retries run at once;
			// the last refusal names 7 s, which must reach the agent as RetryInfo.
			ra := "0"
			if n > 5 {
				ra = "7"
			}
			envelope(w, http.StatusTooManyRequests, "rate_limit_error", "Rate limited", map[string]string{"Retry-After": ra})
		case fault500:
			envelope(w, http.StatusInternalServerError, "api_error", "Internal server error", map[string]string{"Retry-After": "0"})
		case faultReset:
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("no hijacker")
				return
			}
			conn, _, err := hj.Hijack()
			if err == nil {
				_ = conn.Close()
			}
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, okSSE)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// faultGatewayServer wires a Server whose gateway serves a lease on m1 (falling
// back to m2 when m2Endpoint is set and declared equivalent) through a real
// llm.Provider, with the step-output stream on, so the model call is the streamed
// one an agent's native turn makes in production. The breaker opens after ONE
// failure, so a failure that counted against health would refuse the next call
// without a request.
func faultGatewayServer(t *testing.T, m1Endpoint, m2Endpoint string) (*Server, domain.LeaseID, context.Context) {
	t.Helper()
	gens := []config.GeneratorConfig{{ID: "m1", Provider: "openai", Model: "m", Endpoint: m1Endpoint, TimeoutMs: 5000, NativeTools: true}}
	cfg := config.LLMProviderConfig{Default: "m1", Health: config.HealthConfig{FailureThreshold: 1, CooldownMs: 60000}}
	alloc := domain.StepAllocation{Winner: domain.AgentDefinition{ID: "llm:m1"}}
	if m2Endpoint != "" {
		gens = append(gens, config.GeneratorConfig{ID: "m2", Provider: "openai", Model: "m", Endpoint: m2Endpoint, TimeoutMs: 5000, NativeTools: true})
		cfg.EquivalentGenerators = [][]string{{"m1", "m2"}}
		alloc.Fallbacks[0] = domain.AgentDefinition{ID: "llm:m2"}
	}
	cfg.Generators = gens
	provider, err := llm.NewProvider(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, gw := newLeaseServer(t)
	gw.SetStepModels(provider)
	lease, err := gw.Acquire(context.Background(), alloc, 4096, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	gw.BindLease(lease, domain.LeaseBinding{SessionID: "s1", StepIndex: 0, AgentID: "a1"})
	tl := &timeline{}
	s.StepOutput = stepoutput.New(stepoutput.Options{Leases: gw, Sinks: []stepoutput.Sink{tl.sink}})
	s.LLMExchangeSink = tl.exchange
	return s, lease, ctxWithMD("x-agent-id", "a1")
}

func turn(s *Server, ctx context.Context, lease domain.LeaseID) (*pb.GenerateWithToolsResponse, error) {
	return s.GenerateWithTools(ctx, &pb.GenerateWithToolsRequest{
		LeaseId:  string(lease),
		Messages: []*pb.ModelMessageProto{{Role: "user", Content: "say hello"}},
		Tools:    []*pb.ToolDefinitionProto{{Name: "final_answer", ParametersJson: `{}`}},
	})
}

// A refused credential — as a 401 envelope or streamed after a 200 by a proxy —
// is Unauthenticated, is asked once, is never served by the equivalent model, and
// does not open the breaker: the next call reaches the provider again.
func TestGatewayE2E_ProviderAuthFailureIsUnauthenticatedWithoutRetryOrFallback(t *testing.T) {
	t.Parallel()
	for _, fault := range []string{faultAuth, faultAuthStream} {
		t.Run(fault, func(t *testing.T) {
			t.Parallel()
			m1, m1Hits := faultServer(t, fault)
			m2, m2Hits := faultServer(t, "")
			s, lease, ctx := faultGatewayServer(t, m1.URL, m2.URL)

			for call := 1; call <= 2; call++ {
				_, err := turn(s, ctx, lease)
				if got := status.Code(err); got != codes.Unauthenticated {
					t.Fatalf("call %d: code = %v (%v), want Unauthenticated", call, got, err)
				}
				if got := m1Hits.Load(); got != int32(call) {
					t.Fatalf("call %d: provider saw %d requests, want %d (no retry; breaker not opened)", call, got, call)
				}
			}
			if m2Hits.Load() != 0 {
				t.Fatalf("a refused credential fell back to the equivalent model (%d requests)", m2Hits.Load())
			}
		})
	}
}

// An endpoint failure still falls back to an equivalent model (ADR-0139 D1 is
// unchanged for outages): the turn succeeds on m2.
func TestGatewayE2E_EndpointFailureStillFallsBack(t *testing.T) {
	t.Parallel()
	m1, m1Hits := faultServer(t, fault500)
	m2, m2Hits := faultServer(t, "")
	s, lease, ctx := faultGatewayServer(t, m1.URL, m2.URL)
	resp, err := turn(s, ctx, lease)
	if err != nil || !strings.Contains(resp.GetText(), "hello") {
		t.Fatalf("turn = %+v, %v; want m2's reply", resp, err)
	}
	if m1Hits.Load() == 0 || m2Hits.Load() != 1 {
		t.Fatalf("m1 hits %d, m2 hits %d; want m1 tried, m2 serving once", m1Hits.Load(), m2Hits.Load())
	}
}

// With nothing to fall back to, each outage class reaches the agent with the
// status it can act on, never Internal.
func TestGatewayE2E_ProviderOutageClassesMapToActionableCodes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		fault     string
		want      codes.Code
		wantHits  int32 // 0 = at least one
		wantRetry time.Duration
	}{
		// The transport's backoff: the first request and five retries.
		{fault: fault429, want: codes.ResourceExhausted, wantHits: 6, wantRetry: 7 * time.Second},
		{fault: fault500, want: codes.Unavailable, wantHits: 6},
		{fault: faultReset, want: codes.Unavailable},
	}
	for _, c := range cases {
		t.Run(c.fault, func(t *testing.T) {
			t.Parallel()
			m1, hits := faultServer(t, c.fault)
			s, lease, ctx := faultGatewayServer(t, m1.URL, "")
			_, err := turn(s, ctx, lease)
			if got := status.Code(err); got != c.want {
				t.Fatalf("code = %v (%v), want %v", got, err, c.want)
			}
			if c.wantHits > 0 && hits.Load() != c.wantHits {
				t.Errorf("provider saw %d requests, want %d", hits.Load(), c.wantHits)
			}
			if c.wantHits == 0 && hits.Load() < 1 {
				t.Errorf("provider saw no request")
			}
			if got := retryDelay(err); got != c.wantRetry {
				t.Errorf("RetryInfo = %v, want %v", got, c.wantRetry)
			}
		})
	}
}
