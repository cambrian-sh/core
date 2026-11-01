package health

import (
	"net/http"
)

// HealthzHandler is the /healthz shim for dumb probes that cannot speak gRPC:
// 200 when the kernel is ready, 503 otherwise. PLAT-03 / ADR-0065.
//
// The package returns a HANDLER, not a server. ADR-0130 D3: every TCP listener
// the kernel opens is built by app/listener_security.go (bind address, loopback
// default, routable-plaintext refusal, TLS, timeouts, body cap), and this
// package cannot import app (app imports internal/*), so the composition root
// serves this handler through serveHTTP. The shim used to build its own
// http.Server on ":port" — every interface — which is exactly the half-truth
// the listener policy closed (T-20261101-remaining-http-server-literals-outside-listener).
func (c *Checker) HealthzHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if c.Ready() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready\n"))
	})
	return mux
}
