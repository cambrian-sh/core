package llm

import (
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"time"
)

// sharedLLMTransport is the connection-pooled HTTP transport every LLM client reuses, so
// concurrent LLM requests reuse warm TLS connections instead of churning a fresh handshake
// per call. Go's http.DefaultTransport keeps only 2 idle connections per host, so under
// sustained parallel LLM traffic — interviews (unbounded errgroup), concurrent DAG steps,
// the planner — most calls would otherwise pay a full TLS handshake every time. This does
// NOT cap concurrency (MaxConnsPerHost = 0 = unlimited); it only pools idle connections.
//
// It is wrapped by rateLimitRetryTransport so that a provider's HTTP 429 (Too Many
// Requests) / 503 is backed off and retried transparently instead of surfacing as a hard
// failure. Cambrian fans out many LLM calls per turn (planner + verifier + per-step agents
// + agentic-retrieval sub-queries), several of which bypass the LLMGateway CONWIP
// semaphore and hit the provider directly; without backoff, a rate-limited endpoint returns
// 429 and the calls cascade into DEADLINE_EXCEEDED. Every provider client (openai,
// anthropic, gemini, ollama) reuses this transport, so the retry applies uniformly.
var sharedLLMTransport http.RoundTripper = func() http.RoundTripper {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &rateLimitRetryTransport{base: http.DefaultTransport}
	}
	t := base.Clone()
	t.MaxIdleConns = 256
	t.MaxIdleConnsPerHost = 64 // reuse many warm connections to one provider host
	t.MaxConnsPerHost = 0      // do NOT cap concurrent connections — parallelism is preserved
	t.IdleConnTimeout = 90 * time.Second
	return &rateLimitRetryTransport{base: t}
}()

const (
	llmMaxRateLimitRetries = 5
	llmRetryBaseBackoff    = 500 * time.Millisecond
	llmRetryMaxBackoff     = 20 * time.Second
)

// isRetryableStatus reports whether a provider HTTP status is a transient failure worth
// retrying: 429 (rate limit) plus the 5xx the shared hosted endpoint intermittently emits
// under sustained load (500/502/503/504). A transient 500 must not surface as a hard agent
// failure — it is endpoint weather, not a wrong answer.
func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	default:
		return false
	}
}

// rateLimitRetryTransport retries provider 429/503 responses with capped exponential
// backoff (honoring a Retry-After header when present), rewinding the request body via
// GetBody. It gives up after llmMaxRateLimitRetries and hands the last 429 back to the
// caller so the existing health/circuit-breaker path still applies. Backoff waits respect
// the request context deadline.
type rateLimitRetryTransport struct {
	base http.RoundTripper
}

func (rt *rateLimitRetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		// Rewind the body for retries. Only possible when the client set GetBody
		// (all LLM clients build the request from a bytes.Buffer, so it is set).
		if attempt > 0 && req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			req.Body = body
		}

		resp, err := rt.base.RoundTrip(req)
		if err != nil {
			return resp, err
		}
		if !isRetryableStatus(resp.StatusCode) {
			return resp, nil
		}
		// Cannot safely replay a body-less-rewind request, or retries exhausted:
		// return the rate-limit response and let the caller's health path handle it.
		if attempt >= llmMaxRateLimitRetries || (req.Body != nil && req.GetBody == nil) {
			return resp, nil
		}

		wait := retryBackoff(attempt, resp.Header.Get("Retry-After"))
		// Drain + close so the pooled connection can be reused for the retry.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()

		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-req.Context().Done():
			timer.Stop()
			return nil, req.Context().Err()
		}
	}
}

// retryBackoff returns the wait before the next attempt: the Retry-After header when the
// server supplies one (seconds), else capped exponential backoff with full jitter.
func retryBackoff(attempt int, retryAfter string) time.Duration {
	if retryAfter != "" {
		if secs, err := strconv.Atoi(retryAfter); err == nil && secs >= 0 {
			d := time.Duration(secs) * time.Second
			if d > llmRetryMaxBackoff {
				d = llmRetryMaxBackoff
			}
			return d
		}
	}
	backoff := llmRetryBaseBackoff << uint(attempt) // 0.5s, 1s, 2s, 4s, 8s
	if backoff > llmRetryMaxBackoff {
		backoff = llmRetryMaxBackoff
	}
	// Full jitter spreads a thundering herd of concurrent retries.
	return time.Duration(rand.Int63n(int64(backoff) + 1))
}
