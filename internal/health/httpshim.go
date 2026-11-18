package health

import (
	"fmt"
	"net/http"
)

// HealthzHandler is the /healthz shim for dumb probes that cannot speak gRPC:
// 200 when the kernel is ready, 503 otherwise. PLAT-03 / ADR-0065.
//
// The body names each registered component after the first line ("chat: ready" or
// "chat: not ready: <reason>"); a component never changes the status code here, so a
// probe of /healthz keeps meaning "the kernel serves". /healthz?component=<name> asks
// about one component alone: 200 when the kernel and it are ready, 503 with the reason
// otherwise, 404 for a name nobody registered (ENG-2105).
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
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if name := r.URL.Query().Get("component"); name != "" {
			st, ok := c.Component(name)
			switch {
			case !ok:
				w.WriteHeader(http.StatusNotFound)
				_, _ = fmt.Fprintf(w, "%s: unknown component\n", name)
			case !c.Ready():
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = fmt.Fprintf(w, "%s: not ready: kernel not ready\n", name)
			case !st.Ready:
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = fmt.Fprintln(w, componentLine(st))
			default:
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprintln(w, componentLine(st))
			}
			return
		}
		comps := c.Components()
		if c.Ready() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready\n"))
		}
		for _, st := range comps {
			_, _ = fmt.Fprintln(w, componentLine(st))
		}
	})
	return mux
}

func componentLine(st ComponentStatus) string {
	if st.Ready {
		return st.Name + ": ready"
	}
	return st.Name + ": not ready: " + st.Reason
}
