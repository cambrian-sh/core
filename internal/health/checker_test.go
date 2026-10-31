package health_test

import (
	"context"
	"errors"
	"testing"
	"time"

	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/cambrian-sh/core/internal/health"
)

func status(t *testing.T, c *health.Checker, service string) healthpb.HealthCheckResponse_ServingStatus {
	t.Helper()
	resp, err := c.Server().Check(context.Background(), &healthpb.HealthCheckRequest{Service: service})
	if err != nil {
		t.Fatalf("health Check(%q): %v", service, err)
	}
	return resp.GetStatus()
}

// Before any probe, the kernel is NOT_SERVING (nothing reports ready prematurely).
func TestChecker_InitiallyNotServing(t *testing.T) {
	c := health.New(func(context.Context) error { return nil })
	if got := status(t, c, ""); got != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("expected NOT_SERVING before Start, got %v", got)
	}
	if c.Ready() {
		t.Fatal("Ready() should be false before Start")
	}
}

// A passing probe flips the overall + operator status to SERVING.
func TestChecker_ProbeOK_Serving(t *testing.T) {
	c := health.New(func(context.Context) error { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx, time.Hour) // Start runs one synchronous probe immediately
	if got := status(t, c, ""); got != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("expected SERVING after a passing probe, got %v", got)
	}
	if got := status(t, c, health.OperatorServiceName); got != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("operator service should be SERVING, got %v", got)
	}
	if !c.Ready() {
		t.Fatal("Ready() should be true after a passing probe")
	}
}

// A failing probe (e.g. Postgres down) keeps/sets NOT_SERVING.
func TestChecker_ProbeFails_NotServing(t *testing.T) {
	c := health.New(func(context.Context) error { return errors.New("db down") })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx, time.Hour)
	if got := status(t, c, ""); got != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("expected NOT_SERVING when the DB probe fails, got %v", got)
	}
	if c.Ready() {
		t.Fatal("Ready() should be false when the probe fails")
	}
}

// Shutdown drains: NOT_SERVING, and a later probe cannot flip it back to SERVING.
func TestChecker_ShutdownDrains(t *testing.T) {
	c := health.New(func(context.Context) error { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx, time.Hour)
	if got := status(t, c, ""); got != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("precondition: expected SERVING, got %v", got)
	}
	c.Shutdown()
	if got := status(t, c, ""); got != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("expected NOT_SERVING after Shutdown, got %v", got)
	}
	// A probe after Shutdown must not resurrect SERVING.
	c.Start(ctx, time.Hour)
	if got := status(t, c, ""); got != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("Shutdown must be sticky, got %v", got)
	}
}

// Shutdown landing while a probe is IN FLIGHT must stay sticky: the probe was admitted
// before the drain began, but its passing result must not flip Ready() (read by the
// /healthz shim) back to true afterwards. grpc's health.Server already ignores the late
// SetServingStatus, so only Ready() could lie here
// (T-20261031-health-checker-can-set-ready-true-after, ADR-0065).
func TestChecker_ShutdownDuringInFlightProbe_StaysNotReady(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	c := health.New(func(context.Context) error {
		close(started)
		<-release
		return nil // the probe PASSES, but only after the drain began
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	finished := make(chan struct{})
	go func() {
		c.Start(ctx, time.Hour) // runs the blocking probe synchronously
		close(finished)
	}()
	<-started
	c.Shutdown()
	close(release)
	<-finished

	if c.Ready() {
		t.Fatal("Ready() flipped back to true after Shutdown: /healthz would answer 200 while draining")
	}
	if got := status(t, c, ""); got != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("expected NOT_SERVING after Shutdown, got %v", got)
	}
}
