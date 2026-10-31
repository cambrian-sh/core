package health_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/cambrian-sh/core/internal/health"
	"github.com/cambrian-sh/core/internal/testing/chaos"
)

// R2 in the debugging playbook: the gRPC port is open but the kernel is not
// serving until initialisation completes
// (T-20261031-documented-production-failure-modes-have-no). The kernel-level
// ordering (app.go registers the health server and calls Health.Start only
// after NewPgVectorAdapter and the plugin/MCP wiring) cannot be driven without
// a Postgres; what CAN be pinned without one is the Checker contract the
// ordering relies on, and this file does that.
//
// Guards checker.go: New() `c.set(false)` (NOT_SERVING before the first probe),
// runProbe `ok = err == nil` → set(ok), and the 3 s `context.WithTimeout` that
// bounds a hung probe.

// A readiness probe that fails until init completes keeps the kernel
// NOT_SERVING, flips it to SERVING on the first passing probe, and flips it
// back when the dependency is lost.
func TestChaos_Readiness_NotServingUntilInitCompletes(t *testing.T) {
	var initDone atomic.Bool
	c := health.New(func(context.Context) error {
		if initDone.Load() {
			return nil
		}
		return errors.New("init not complete")
	})
	if got := status(t, c, ""); got != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("before Start: want NOT_SERVING, got %v", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx, 20*time.Millisecond)
	if got := status(t, c, ""); got != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("probe failing: want NOT_SERVING, got %v", got)
	}
	if c.Ready() {
		t.Fatal("Ready() must be false while the probe fails")
	}

	initDone.Store(true)
	chaos.WaitUntil(t, 3*time.Second, "SERVING after init completes", func() bool {
		return status(t, c, "") == healthpb.HealthCheckResponse_SERVING &&
			status(t, c, health.OperatorServiceName) == healthpb.HealthCheckResponse_SERVING &&
			c.Ready()
	})

	initDone.Store(false) // dependency lost (DB down)
	chaos.WaitUntil(t, 3*time.Second, "NOT_SERVING after the dependency is lost", func() bool {
		return status(t, c, "") == healthpb.HealthCheckResponse_NOT_SERVING && !c.Ready()
	})
}

// A probe that hangs (a wedged DB pool) must not hang Start or report SERVING:
// the probe runs under a bounded context and a timeout counts as not ready.
func TestChaos_Readiness_HungProbeIsBoundedAndNotServing(t *testing.T) {
	c := health.New(func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	start := time.Now()
	c.Start(ctx, time.Hour) // the immediate probe must return on its own timeout
	elapsed := time.Since(start)
	if elapsed > 10*time.Second {
		t.Fatalf("Start blocked on a hung probe for %s; the probe timeout did not fire", elapsed)
	}
	if got := status(t, c, ""); got != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("a hung probe must read as NOT_SERVING, got %v", got)
	}
	if c.Ready() {
		t.Fatal("Ready() must be false after a hung probe")
	}
}
