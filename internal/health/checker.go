// Package health maintains the standard grpc.health.v1 serving status of the kernel
// from a readiness probe (PLAT-03 / ADR-0065). Readiness is: the process is up (bbolt
// open, operator service registered — implied by the server serving) AND the database
// is reachable. It is deliberately NOT gated on agents — agents degrade independently
// and expose their own status. A background loop keeps the status live; a Shutdown call
// flips it to NOT_SERVING so a drain is observable to probes and load balancers.
//
// Lanes that depend on agents report through named components (AddComponent): the chat
// lane is not ready while its pool waits for the sandbox Python env (ENG-2105). A
// component never moves the overall status — ADR-0065's readiness stays agent-free, so
// the container HEALTHCHECK and setup's probe keep meaning "the kernel serves" — but it
// is visible: as its own grpc.health.v1 service (ComponentServicePrefix+name), on the
// /healthz body, and alone at /healthz?component=<name>.
package health

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// OperatorServiceName is the health key for the OperatorConsole surface, set alongside
// the overall ("") status so a probe can target it specifically.
const OperatorServiceName = "cambrian.OperatorConsole"

// ComponentServicePrefix prefixes a component's grpc.health.v1 service name: the chat
// lane is "cambrian.lane.chat".
const ComponentServicePrefix = "cambrian.lane."

// ComponentStatus is one component's readiness at the moment it was asked.
type ComponentStatus struct {
	Name   string
	Ready  bool
	Reason string // why it is not ready; empty when ready
}

type component struct {
	name  string
	ready func() error
}

// Checker drives the grpc health server from a readiness probe.
type Checker struct {
	srv   *health.Server
	probe func(context.Context) error // nil ⇒ always ready (process-liveness only)
	ready atomic.Bool
	done  atomic.Bool

	// mu makes "check done, then store ready" atomic against Shutdown. Without it a
	// probe admitted before Shutdown (runProbe's done check passes, then the probe
	// runs for up to 3s) would store ready=true AFTER Shutdown stored false, and the
	// /healthz shim would answer 200 while draining — breaking ADR-0065's sticky
	// NOT_SERVING (T-20261031-health-checker-can-set-ready-true-after). grpc's
	// health.Server already ignores a SetServingStatus after its own Shutdown; only
	// our ready flag needed the guard.
	mu sync.Mutex

	compMu     sync.RWMutex
	components []component
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

// AddComponent registers a named readiness (e.g. "chat"): ready returns nil when the
// component serves, else the reason it does not. It is called on every probe tick and
// every /healthz request, so it must answer at once (read state; never do I/O or wait
// on a lock held across I/O). Registering a name twice replaces the earlier probe.
func (c *Checker) AddComponent(name string, ready func() error) {
	if name == "" || ready == nil {
		return
	}
	c.compMu.Lock()
	defer c.compMu.Unlock()
	for i := range c.components {
		if c.components[i].name == name {
			c.components[i].ready = ready
			return
		}
	}
	c.components = append(c.components, component{name: name, ready: ready})
}

// Components reports every registered component's readiness now, in registration order.
func (c *Checker) Components() []ComponentStatus {
	c.compMu.RLock()
	comps := append([]component(nil), c.components...)
	c.compMu.RUnlock()
	out := make([]ComponentStatus, 0, len(comps))
	for _, comp := range comps {
		st := ComponentStatus{Name: comp.name, Ready: true}
		if err := comp.ready(); err != nil {
			st.Ready, st.Reason = false, err.Error()
		}
		out = append(out, st)
	}
	return out
}

// Component reports one component's readiness now; ok is false for an unknown name.
func (c *Checker) Component(name string) (st ComponentStatus, ok bool) {
	for _, s := range c.Components() {
		if s.Name == name {
			return s, true
		}
	}
	return ComponentStatus{}, false
}

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
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done.Load() {
		return // a probe that finished after Shutdown must not resurrect readiness
	}
	c.ready.Store(ok)
	status := healthpb.HealthCheckResponse_NOT_SERVING
	if ok {
		status = healthpb.HealthCheckResponse_SERVING
	}
	c.srv.SetServingStatus("", status)
	c.srv.SetServingStatus(OperatorServiceName, status)
	// A component serves only while the kernel does and it reports ready.
	for _, comp := range c.Components() {
		cs := healthpb.HealthCheckResponse_NOT_SERVING
		if ok && comp.Ready {
			cs = healthpb.HealthCheckResponse_SERVING
		}
		c.srv.SetServingStatus(ComponentServicePrefix+comp.Name, cs)
	}
}

// Shutdown marks NOT_SERVING (drain). Call before GracefulStop so probes and load
// balancers stop routing to a draining kernel. Idempotent; readiness cannot recover
// after it.
func (c *Checker) Shutdown() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.done.Store(true)
	c.ready.Store(false)
	c.srv.Shutdown() // sets every registered service to NOT_SERVING
}
