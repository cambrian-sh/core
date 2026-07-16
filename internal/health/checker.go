// Package health maintains the standard grpc.health.v1 serving status of the kernel
// from a readiness probe (PLAT-03 / ADR-0065). Readiness is: the process is up (bbolt
// open, operator service registered — implied by the server serving) AND the database
// is reachable. It is deliberately NOT gated on agents — agents degrade independently
// and expose their own status. A background loop keeps the status live; a Shutdown call
// flips it to NOT_SERVING so a drain is observable to probes and load balancers.
package health

import (
	"context"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// OperatorServiceName is the health key for the OperatorConsole surface, set alongside
// the overall ("") status so a probe can target it specifically.
const OperatorServiceName = "cambrian.OperatorConsole"

// Checker drives the grpc health server from a readiness probe.
type Checker struct {
	srv   *health.Server
	probe func(context.Context) error // nil ⇒ always ready (process-liveness only)
	ready atomic.Bool
	done  atomic.Bool
}

// New builds a Checker. probe is the readiness check (typically a DB ping); a nil probe
// means readiness is pure process-liveness. The initial status is NOT_SERVING until the
// first probe runs (Start), so nothing reports SERVING before it is actually ready.
func New(probe func(context.Context) error) *Checker {
	c := &Checker{srv: health.NewServer(), probe: probe}
	c.set(false)
	return c
}

// Server returns the grpc health server to register on the gRPC server.
func (c *Checker) Server() *health.Server { return c.srv }

// Ready reports the last-observed readiness (for the HTTP /healthz shim).
func (c *Checker) Ready() bool { return c.ready.Load() }

// Start runs an immediate probe, then re-probes every interval until ctx is cancelled.
func (c *Checker) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	c.runProbe(ctx)
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c.runProbe(ctx)
			}
		}
	}()
}

func (c *Checker) runProbe(ctx context.Context) {
	if c.done.Load() {
		return // never flip back to SERVING after a Shutdown
	}
	ok := true
	if c.probe != nil {
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := c.probe(pctx)
		cancel()
		ok = err == nil
	}
	c.set(ok)
}

func (c *Checker) set(ok bool) {
	c.ready.Store(ok)
	status := healthpb.HealthCheckResponse_NOT_SERVING
	if ok {
		status = healthpb.HealthCheckResponse_SERVING
	}
	c.srv.SetServingStatus("", status)
	c.srv.SetServingStatus(OperatorServiceName, status)
}

// Shutdown marks NOT_SERVING (drain). Call before GracefulStop so probes and load
// balancers stop routing to a draining kernel. Idempotent; readiness cannot recover
// after it.
func (c *Checker) Shutdown() {
	c.done.Store(true)
	c.ready.Store(false)
	c.srv.Shutdown() // sets every registered service to NOT_SERVING
}
