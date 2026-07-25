// Package agentpool provides a bounded pool of interchangeable daemon-agent workers
// (ADR-0084 D4).
//
// It exists because chat previously ran one OS process per conversation. That shape buys
// in-process per-conversation state — which the session agent does not use, since it is
// stateless per call and receives its history threaded into every turn — while paying
// memory, file-descriptor, and spawn-latency costs that grow without bound as concurrency
// grows. A fixed pool inverts that: process count is a configured constant, and concurrency
// is a knob rather than a consequence of how many people happen to be talking.
//
// Because every turn carries its own context, ANY worker can serve ANY turn: dispatch is
// load balancing, not sticky routing, and a lost worker costs nothing but the in-flight
// call. The package is deliberately free of chat concepts so the primitive is reusable.
package agentpool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// Dispatcher is the kernel daemon seam a pool needs, satisfied by *agentmgr.AgentManager.
type Dispatcher interface {
	SpawnDaemon(agentID, streamID string, params map[string]any) (instanceID string, err error)
	CallDaemon(ctx context.Context, streamID string, h *domain.Handoff) (*domain.Handoff, error)
	StopDaemon(streamID string) error
	// DaemonInstanceID reports whether a live daemon is registered for a stream. The pool
	// uses it to tell "the worker never received this turn" from "the worker ran the turn
	// and failed" — a distinction that decides whether a retry is safe.
	DaemonInstanceID(streamID string) (string, bool)
}

var (
	// ErrPoolBusy is returned when the pool is saturated and the wait queue is full. It is
	// a SHED, not a failure: the caller should back off rather than retry immediately.
	ErrPoolBusy = errors.New("agent pool saturated")
	// ErrWorkerLost reports that the chosen worker was not live, so the turn was never
	// delivered. The pool respawns the worker; whether to re-send the turn is the CALLER's
	// decision, deliberately — see Dispatch.
	ErrWorkerLost = errors.New("agent pool worker lost before dispatch")
	// ErrPoolNotStarted is returned by Dispatch before Start (or after Stop).
	ErrPoolNotStarted = errors.New("agent pool not started")
)

// Config configures a pool.
type Config struct {
	// AgentID is the agent every worker runs.
	AgentID string
	// Size is the number of workers, and therefore the maximum number of turns executing
	// at once. Must be >= 1.
	Size int
	// QueueSize bounds how many additional turns may WAIT for a worker. Beyond
	// Size+QueueSize in flight, Dispatch sheds immediately with ErrPoolBusy. An unbounded
	// queue in front of a bounded pool is just a slower crash, so 0 (shed as soon as all
	// workers are busy) is a legitimate, safe setting.
	QueueSize int
	// AcquireTimeout bounds how long an admitted turn waits for a free worker before it is
	// shed. Zero means wait until the caller's context expires.
	AcquireTimeout time.Duration
	// StreamPrefix namespaces the synthetic stream ids of workers. Defaults to "pool".
	StreamPrefix string
	// Params are passed to each worker at spawn.
	Params map[string]any
}

func (c Config) withDefaults() Config {
	if c.StreamPrefix == "" {
		c.StreamPrefix = "pool"
	}
	if c.Size < 1 {
		c.Size = 1
	}
	if c.QueueSize < 0 {
		c.QueueSize = 0
	}
	return c
}

// Pool is a bounded set of interchangeable daemon workers.
type Pool struct {
	cfg  Config
	disp Dispatcher

	// free carries the stream ids of idle workers; its capacity is the pool size.
	free chan string
	// admit bounds total in-flight work (executing + waiting) at Size+QueueSize. Taking a
	// slot is non-blocking, which is what makes shedding immediate rather than queued.
	admit chan struct{}

	mu      sync.Mutex
	workers []string
	started bool
}

// New builds an unstarted pool.
func New(d Dispatcher, cfg Config) *Pool {
	cfg = cfg.withDefaults()
	return &Pool{
		cfg:   cfg,
		disp:  d,
		free:  make(chan string, cfg.Size),
		admit: make(chan struct{}, cfg.Size+cfg.QueueSize),
	}
}

// Size reports the configured worker count.
func (p *Pool) Size() int { return p.cfg.Size }

// Start spawns the workers. It is all-or-nothing: a partial pool would silently serve at
// reduced capacity, so any spawn failure tears down what was already started.
func (p *Pool) Start(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started {
		return nil
	}
	for i := 0; i < p.cfg.Size; i++ {
		stream := fmt.Sprintf("%s-%s-%d", p.cfg.StreamPrefix, p.cfg.AgentID, i)
		if _, err := p.disp.SpawnDaemon(p.cfg.AgentID, stream, p.cfg.Params); err != nil {
			for _, s := range p.workers {
				_ = p.disp.StopDaemon(s)
			}
			p.workers = nil
			return fmt.Errorf("agentpool: spawn worker %d/%d for %q: %w", i+1, p.cfg.Size, p.cfg.AgentID, err)
		}
		p.workers = append(p.workers, stream)
		p.free <- stream
	}
	p.started = true
	slog.Info("ADR-0084: agent pool started",
		"agent", p.cfg.AgentID, "size", p.cfg.Size, "queue", p.cfg.QueueSize)
	return nil
}

// Stop drains the pool and stops every worker.
func (p *Pool) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.started {
		return
	}
	p.started = false
	for _, s := range p.workers {
		if err := p.disp.StopDaemon(s); err != nil {
			slog.Warn("agentpool: stopping worker failed", "stream", s, "err", err)
		}
	}
	p.workers = nil
	// Drain idle markers so a restarted pool does not inherit stale entries.
	for {
		select {
		case <-p.free:
		default:
			return
		}
	}
}

// Dispatch runs one handoff on some free worker.
//
// It does NOT retry. When a worker turns out not to be live, the pool respawns it and
// returns ErrWorkerLost so the caller can decide — because a turn may already have executed
// side-effecting tool calls before the failure, and a blind retry would run them twice. That
// judgement belongs to the component that knows what the turn was (the chat manager, or a
// caller with idempotency of its own), not to a generic pool. Mechanism here, policy there.
func (p *Pool) Dispatch(ctx context.Context, h *domain.Handoff) (*domain.Handoff, error) {
	p.mu.Lock()
	started := p.started
	p.mu.Unlock()
	if !started {
		return nil, ErrPoolNotStarted
	}

	// Admission: immediate shed once Size+QueueSize turns are in flight.
	select {
	case p.admit <- struct{}{}:
	default:
		return nil, ErrPoolBusy
	}
	defer func() { <-p.admit }()

	waitCtx := ctx
	if p.cfg.AcquireTimeout > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, p.cfg.AcquireTimeout)
		defer cancel()
	}

	var stream string
	select {
	case stream = <-p.free:
	case <-waitCtx.Done():
		if ctx.Err() != nil {
			return nil, ctx.Err() // the caller gave up, not a shed
		}
		return nil, ErrPoolBusy
	}
	defer func() { p.free <- stream }()

	resp, err := p.disp.CallDaemon(ctx, stream, h)
	if err != nil {
		if _, live := p.disp.DaemonInstanceID(stream); !live {
			// The worker was gone, so this turn never ran. Self-heal, then tell the
			// caller precisely that — retry safety is theirs to judge.
			if _, respawnErr := p.disp.SpawnDaemon(p.cfg.AgentID, stream, p.cfg.Params); respawnErr != nil {
				slog.Error("agentpool: worker respawn failed", "stream", stream, "err", respawnErr)
			} else {
				slog.Warn("agentpool: worker was lost and has been respawned", "stream", stream)
			}
			return nil, fmt.Errorf("%w: %v", ErrWorkerLost, err)
		}
		return nil, err
	}
	return resp, nil
}
