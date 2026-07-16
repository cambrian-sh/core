package health

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// ServeHealthz runs a tiny HTTP server on :port that returns 200 when the kernel is
// ready and 503 otherwise — for dumb probes that cannot speak gRPC. port <= 0 disables
// it (returns nil immediately). Blocks until ctx is cancelled. PLAT-03 / ADR-0065.
func (c *Checker) ServeHealthz(ctx context.Context, port int) error {
	if port <= 0 {
		return nil
	}
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
	srv := &http.Server{Addr: ":" + strconv.Itoa(port), Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	slog.Info("🩺 healthz HTTP shim listening", "port", port)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
