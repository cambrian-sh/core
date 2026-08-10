package llm

import (
	"bytes"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
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
		// A 429 is only worth retrying when it is a RATE limit — a per-minute ceiling
		// that clears on its own. A QUOTA exhaustion does not clear for days, and
		// retrying it burns the full backoff ladder before failing anyway.
		//
		// Measured 2026-08-07: the provider returned
		// `{"type":"error","error":{"type":"GoUsageLimitError","message":"Weekly usage
		// limit reached. Resets in 2 days."}}` in 0.56s to curl. Through here it became
		// five retries, then a 60s context deadline, and the planner logged NOTHING —
		// so chat looked broken when the provider had answered immediately and clearly.
		// Turning a fast, explicit refusal into a slow, silent one is the defect.
		if resp.StatusCode == http.StatusTooManyRequests {
			if detail, exhausted := quotaExhausted(resp); exhausted {
				slog.Warn("llm: provider refused with a NON-TRANSIENT quota limit — not retrying",
					"host", req.URL.Host,
					"path", req.URL.Path,
					"attempt", attempt,
					"provider_message", detail)
				return resp, nil
			}
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

// quotaMarkers are body substrings that mean "this 429 will not clear on its own".
//
// Matched against the LOWERCASED body. Deliberately specific: "limit" alone would also
// match an ordinary rate limit, which is exactly the case that SHOULD be retried, so a
// loose match here would make the kernel give up on weather it used to ride out.
var quotaMarkers = []string{
	"usage limit",           // opencode.ai GoUsageLimitError
	"usagelimit",            // its error TYPE, spelling-independent
	"quota",                 // openai insufficient_quota, gemini
	"billing",               // "billing hard limit reached"
	"credit balance",        // anthropic
	"exceeded your current", // openai's phrasing
}

// quotaExhausted reports whether a 429 body names a non-transient quota, and returns the
// provider's own message for the log.
//
// It RESTORES resp.Body in every path — the caller (and the provider client's error
// handling) must still be able to read it. A sniffer that consumed the body would trade a
// slow failure for an empty one.
func quotaExhausted(resp *http.Response) (string, bool) {
	if resp == nil || resp.Body == nil {
		return "", false
	}
	const peek = 4 << 10 // enough for any provider error envelope; bounded on purpose
	buf, err := io.ReadAll(io.LimitReader(resp.Body, peek))
	if err != nil && len(buf) == 0 {
		return "", false
	}
	// Put the bytes back, followed by whatever remains unread.
	rest := resp.Body
	resp.Body = struct {
		io.Reader
		io.Closer
	}{Reader: io.MultiReader(bytes.NewReader(buf), rest), Closer: rest}

	lower := strings.ToLower(string(buf))
	for _, m := range quotaMarkers {
		if strings.Contains(lower, m) {
			return strings.TrimSpace(string(buf)), true
		}
	}
	return "", false
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
