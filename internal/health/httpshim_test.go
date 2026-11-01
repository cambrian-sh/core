package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The shim is a handler served by the app's listener policy (ADR-0130 D3); its
// own contract is just 200 when ready, 503 otherwise (ADR-0065).
func TestHealthzHandler_ReadyAndNotReady(t *testing.T) {
	c := New(func(context.Context) error { return nil })
	h := c.HealthzHandler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("before any probe: status %d, want 503", rec.Code)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel) // stops the re-probe goroutine Start launches
	c.Start(ctx, 0)   // runs one probe synchronously before returning
	if !c.Ready() {
		t.Fatal("a passing probe did not make the checker ready")
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("ready: status %d, want 200", rec.Code)
	}
}
