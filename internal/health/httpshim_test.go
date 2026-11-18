package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	healthpb "google.golang.org/grpc/health/grpc_health_v1"
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

// ENG-2105: a component (the chat lane) is reported on the /healthz body, alone at
// /healthz?component=<name> and as its own grpc.health.v1 service — and never moves
// the overall status (ADR-0065 readiness stays agent-free).
func TestHealthzHandler_Components(t *testing.T) {
	c := New(func(context.Context) error { return nil })
	var mu sync.Mutex
	chatErr := errors.New("waiting for the sandbox Python env: building (step: installing)")
	c.AddComponent("chat", func() error {
		mu.Lock()
		defer mu.Unlock()
		return chatErr
	})
	h := c.HealthzHandler()
	get := func(target string) (int, string) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		return rec.Code, rec.Body.String()
	}

	// Kernel not probed yet: the component is not ready either.
	if code, body := get("/healthz?component=chat"); code != http.StatusServiceUnavailable || !strings.Contains(body, "kernel not ready") {
		t.Fatalf("before the first probe: %d %q", code, body)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c.Start(ctx, time.Hour)

	code, body := get("/healthz")
	if code != http.StatusOK || !strings.HasPrefix(body, "ok\n") ||
		!strings.Contains(body, "chat: not ready: waiting for the sandbox Python env: building (step: installing)") {
		t.Fatalf("/healthz while the chat lane waits: %d %q", code, body)
	}
	if code, body := get("/healthz?component=chat"); code != http.StatusServiceUnavailable || !strings.Contains(body, "step: installing") {
		t.Fatalf("component while waiting: %d %q", code, body)
	}
	if code, _ := get("/healthz?component=nope"); code != http.StatusNotFound {
		t.Fatalf("unknown component: %d", code)
	}
	resp, err := c.Server().Check(ctx, &healthpb.HealthCheckRequest{Service: ComponentServicePrefix + "chat"})
	if err != nil || resp.GetStatus() != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("grpc chat lane while waiting: %v %v", resp.GetStatus(), err)
	}

	mu.Lock()
	chatErr = nil
	mu.Unlock()
	if code, body := get("/healthz?component=chat"); code != http.StatusOK || body != "chat: ready\n" {
		t.Fatalf("component once up: %d %q", code, body)
	}
	if code, body := get("/healthz"); code != http.StatusOK || body != "ok\nchat: ready\n" {
		t.Fatalf("/healthz once up: %d %q", code, body)
	}
	c.runProbe(ctx) // the next tick republishes the gRPC component status
	resp, err = c.Server().Check(ctx, &healthpb.HealthCheckRequest{Service: ComponentServicePrefix + "chat"})
	if err != nil || resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("grpc chat lane once up: %v %v", resp.GetStatus(), err)
	}
}
