package llm

import (
	"net/http"
	"time"
)

// sharedLLMTransport is the connection-pooled HTTP transport every LLM client reuses, so
// concurrent LLM requests reuse warm TLS connections instead of churning a fresh handshake
// per call. Go's http.DefaultTransport keeps only 2 idle connections per host, so under
// sustained parallel LLM traffic — interviews (unbounded errgroup), concurrent DAG steps,
// the planner — most calls would otherwise pay a full TLS handshake every time. This does
// NOT cap concurrency (MaxConnsPerHost = 0 = unlimited); it only pools idle connections.
var sharedLLMTransport http.RoundTripper = func() http.RoundTripper {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return http.DefaultTransport
	}
	t := base.Clone()
	t.MaxIdleConns = 256
	t.MaxIdleConnsPerHost = 64 // reuse many warm connections to one provider host
	t.MaxConnsPerHost = 0      // do NOT cap concurrent connections — parallelism is preserved
	t.IdleConnTimeout = 90 * time.Second
	return t
}()
