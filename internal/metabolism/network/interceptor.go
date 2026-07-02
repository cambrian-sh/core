package network

import (
	"log/slog"
	"net/http"
)

// WASIInterceptor intercepts WASI-HTTP calls from Wasm agents.
// It implements the http.RoundTripper interface.
type WASIInterceptor struct {
	pool *HotClientPool
}

// NewWASIInterceptor creates a new WASIInterceptor.
func NewWASIInterceptor(pool *HotClientPool) *WASIInterceptor {
	return &WASIInterceptor{
		pool: pool,
	}
}

// RoundTrip intercepts the request and routes it through the Hot-Client Pool.
func (i *WASIInterceptor) RoundTrip(req *http.Request) (*http.Response, error) {
	slog.Info("Deep Kernel Intercepted I/O", 
		"method", req.Method, 
		"url", req.URL.String(),
		"agent_context", "Deep Kernel Lens",
	)

	client := i.pool.GetClient(req.URL.Host)
	
	// Ensure we use the pool's transport
	return client.Transport.RoundTrip(req)
}
