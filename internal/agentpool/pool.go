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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cambrian-sh/core/domain"
)

// Dispatcher is the kernel daemon seam a pool needs, satisfied by *agentmgr.AgentManager.
type Dispatcher interface {
	SpawnDaemon(ctx context.Context, agentID, streamID string, params map[string]any) (instanceID string, err error)
	CallDaemon(ctx context.Context, streamID string, h *domain.Handoff) (*domain.Handoff, error)
	StopDaemon(streamID string) error
	// DaemonInstanceID reports whether a live daemon is registered for a stream. The pool
	// uses it to tell "the worker never received this turn" from "the worker ran the turn
	// and failed" — a distinction that decides whether a retry is safe.
	DaemonInstanceID(streamID string) (string, bool)
	// DaemonServing reports whether the stream's live worker is heartbeat-tracked
	// (ADR-0070 D3) and whether it reported itself idle after since. The pool uses it
	// to keep a worker whose turn it abandoned out of rotation until it can serve
	// again; tracked=false (an older SDK, liveness off) means "unknowable" and the
	// worker goes straight back, as before.
	DaemonServing(streamID string, since time.Time) (tracked, idle bool)
	// DaemonQuarantined reports whether the restart policy quarantined the stream as a
	// crash-loop (ADR-0070): auto-restart is withdrawn, so no replacement will boot
	// until an operator acts. AwaitReplacement uses it to stop waiting at once
	// (ENG-2269) instead of until the caller's deadline.
	DaemonQuarantined(streamID string) bool
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
	// ErrWorkerQuarantined reports that the worker a caller waits on was quarantined
	// by the restart policy as a crash-loop (ADR-0070): no replacement will boot by
	// itself, so waiting longer cannot help (ENG-2269).
	ErrWorkerQuarantined = errors.New("agent pool worker quarantined: auto-restart withdrawn")
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
	// RecoverTimeout bounds how long a worker whose turn was abandoned (the caller's
	// context ended while the worker still ran it) is held out of rotation, waiting to
	// be replaced (heartbeat liveness killed it and the restart policy respawned it)
	// or to report itself idle again. A single-threaded worker still running an
	// orphaned turn cannot serve the next one; handing it back at once made every
	// later turn on the pool queue behind the hang (ENG-1348). Past the bound the
	// worker is returned anyway. 0 disables the hold (the pre-ENG-1348 behaviour).
	RecoverTimeout time.Duration
	// RecoverPoll is how often a held worker is re-checked. Defaults to 100 ms.
	RecoverPoll time.Duration
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
	if c.RecoverPoll <= 0 {
		c.RecoverPoll = 100 * time.Millisecond
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
	// up mirrors started for readers that must not wait on mu: Start holds mu
	// for the whole spawn (a cold boot is seconds), and a health probe asking
	// "is the lane up?" must answer at once (ENG-2105).
	up atomic.Bool
	// stopRecover ends the recovery goroutines (recoverWorker) at Stop; recovering
	// counts them so Stop waits (AGENTS.md §3.3: the pool owns them).
	stopRecover chan struct{}
	recovering  sync.WaitGroup
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

// Started reports whether the workers are up: Start succeeded and Stop has
// not run since. It never blocks on a Start in progress.
func (p *Pool) Started() bool { return p.up.Load() }

// Size reports the configured worker count.
func (p *Pool) Size() int { return p.cfg.Size }

// Start spawns the workers. It is all-or-nothing: a partial pool would silently serve at
// reduced capacity, so any spawn failure tears down what was already started.
func (p *Pool) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started {
		return nil
	}
	for i := 0; i < p.cfg.Size; i++ {
		stream := fmt.Sprintf("%s-%s-%d", p.cfg.StreamPrefix, p.cfg.AgentID, i)
		if _, err := p.disp.SpawnDaemon(ctx, p.cfg.AgentID, stream, p.cfg.Params); err != nil {
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
	p.up.Store(true)
	p.stopRecover = make(chan struct{})
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
	p.up.Store(false)
	if p.stopRecover != nil {
		close(p.stopRecover)
		p.stopRecover = nil
	}
	// Recovery goroutines put a worker back only while the pool is started (checked
	// under mu), so they exit without touching free; wait for them outside mu.
	p.mu.Unlock()
	p.recovering.Wait()
	p.mu.Lock()
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
	release := true
	defer func() {
		if release {
			p.free <- stream
		}
	}()

	resp, err := p.disp.CallDaemon(ctx, stream, h)
	if err != nil {
		instanceID, live := p.disp.DaemonInstanceID(stream)
		failedAt := time.Now()
		if live && ctx.Err() != nil && p.cfg.RecoverTimeout > 0 {
			// The caller gave up (deadline, stall cut, cancellation) while the worker is
			// still alive — and so, being single-threaded, still busy with this turn.
			// Hold it until it is replaced or reports idle, so the next turn waits for
			// a worker that can serve instead of queueing behind this one (ENG-1348).
			release = false
			p.holdWorker(stream, instanceID, time.Now())
		}
		if !live {
			// The worker was gone, so this turn never ran. Self-heal, then tell the
			// caller precisely that — retry safety is theirs to judge.
			// The self-heal must finish even if this turn's caller gives up: detach
			// its cancellation, keep its values (§2.7).
			if _, respawnErr := p.disp.SpawnDaemon(context.WithoutCancel(ctx), p.cfg.AgentID, stream, p.cfg.Params); respawnErr != nil {
				slog.Error("agentpool: worker respawn failed", "stream", stream, "err", respawnErr)
			} else {
				slog.Warn("agentpool: worker was lost and has been respawned", "stream", stream)
			}
			return nil, &WorkerFailure{Stream: stream, FailedAt: failedAt,
				Err: fmt.Errorf("%w: %w", ErrWorkerLost, err)}
		}
		return nil, &WorkerFailure{Stream: stream, InstanceID: instanceID, FailedAt: failedAt, Err: err}
	}
	return resp, nil
}

// holdWorker keeps stream out of rotation on a pool-owned goroutine until its worker is
// replaced by another instance, reports itself idle after abandonedAt, turns out to be
// untracked by heartbeat liveness (nothing to wait for), or RecoverTimeout passes.
// Then it goes back to free — unless the pool stopped meanwhile.
func (p *Pool) holdWorker(stream, instanceID string, abandonedAt time.Time) {
	p.mu.Lock()
	stop := p.stopRecover
	if !p.started || stop == nil {
		p.mu.Unlock()
		return
	}
	p.recovering.Add(1)
	p.mu.Unlock()

	go func() {
		defer p.recovering.Done()
		deadline := time.NewTimer(p.cfg.RecoverTimeout)
		defer deadline.Stop()
		tick := time.NewTicker(p.cfg.RecoverPoll)
		defer tick.Stop()
		reason := ""
		for reason == "" {
			reason = p.workerReady(stream, instanceID, abandonedAt)
			if reason != "" {
				break
			}
			select {
			case <-stop:
				return
			case <-deadline.C:
				reason = "recover timeout"
			case <-tick.C:
			}
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if !p.started || p.stopRecover != stop {
			return // stopped (or stopped and restarted): Stop drained free already
		}
		slog.Info("agentpool: worker back in rotation after an abandoned turn",
			"stream", stream, "reason", reason, "held", time.Since(abandonedAt).Round(time.Millisecond))
		p.free <- stream
	}()
}

// workerReady returns why a held worker may go back ("" = keep holding).
func (p *Pool) workerReady(stream, instanceID string, abandonedAt time.Time) string {
	current, live := p.disp.DaemonInstanceID(stream)
	if !live {
		return "" // killed; the restart policy is bringing up its replacement
	}
	if current != instanceID {
		return "replaced"
	}
	tracked, idle := p.disp.DaemonServing(stream, abandonedAt)
	if !tracked {
		return "not heartbeat-tracked"
	}
	if idle {
		return "idle again"
	}
	return ""
}

// WorkerFailure is a Dispatch error that names the worker it failed on (ENG-2266):
// the stream, the instance that was live on it when the call failed ("" when none
// was), and when. Error and Unwrap are the underlying error's, so errors.Is,
// errors.As and status.Code see exactly what they saw before.
type WorkerFailure struct {
	Stream     string
	InstanceID string
	FailedAt   time.Time
	Err        error
}

func (f *WorkerFailure) Error() string { return f.Err.Error() }
func (f *WorkerFailure) Unwrap() error { return f.Err }

// NotDelivered reports whether a Dispatch error means the turn never reached a
// worker (ENG-2266), so a caller re-sending a turn treats it as "not delivered yet",
// not as a worker lost under the turn. Two causes qualify:
//
//   - the daemon layer refused before calling anything: the stream had no live
//     instance (domain.ErrDaemonUnavailable, from AgentManager.CallDaemon);
//   - the transport could not be dialled (a dead worker's socket), recognised by
//     grpc-go's own wording for a failed dial ("Error while dialing"), the only
//     signal the transport gives for it.
//
// ErrWorkerLost alone does NOT qualify: the pool returns it whenever the stream has
// no live instance after a failed call, which is also what a worker killed mid-turn
// looks like once the crash watcher has deregistered it; it wraps its cause (%w), and
// the cause decides.
func NotDelivered(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, domain.ErrDaemonUnavailable) {
		return true
	}
	return status.Code(err) == codes.Unavailable && strings.Contains(err.Error(), "Error while dialing")
}

// AwaitReplacement waits, bounded by ctx, until the worker a failed Dispatch named
// (err carries a *WorkerFailure) can serve again (ENG-2266). A worker killed under a
// turn stays registered — "live" — until the crash watcher sees its exit, and then
// is absent until the restart policy boots its replacement; a turn re-sent in that
// window dials a dead socket. Ready means the stream has a live instance other than
// the one that failed (replaced or respawned), or the same instance reported itself
// idle by heartbeat after the failure. It is holdWorker's readiness check
// (workerReady) without its "not heartbeat-tracked" shortcut: an abandoned worker
// that is untracked is merely unknowable, while a worker whose transport broke is
// dead, so only a replacement or a fresh idle heartbeat proves it can serve.
// A nil return also covers an error that names no worker.
//
// A stream the restart policy quarantined as a crash-loop gets no replacement until
// an operator acts, so AwaitReplacement returns ErrWorkerQuarantined at once instead
// of waiting out ctx (ENG-2269; owner decision 2026-11-20). Readiness is checked
// first: a stream an operator already respawned serves again.
func (p *Pool) AwaitReplacement(ctx context.Context, err error) error {
	var wf *WorkerFailure
	if !errors.As(err, &wf) || wf.Stream == "" {
		return nil
	}
	tick := time.NewTicker(p.cfg.RecoverPoll)
	defer tick.Stop()
	for {
		if p.replacementReady(wf) {
			return nil
		}
		if p.disp.DaemonQuarantined(wf.Stream) {
			return fmt.Errorf("agentpool: worker %q: %w", wf.Stream, ErrWorkerQuarantined)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func (p *Pool) replacementReady(wf *WorkerFailure) bool {
	current, live := p.disp.DaemonInstanceID(wf.Stream)
	if !live {
		return false // killed; the restart policy is bringing up its replacement
	}
	if current != wf.InstanceID {
		return true // replaced (or, after ErrWorkerLost, respawned)
	}
	tracked, idle := p.disp.DaemonServing(wf.Stream, wf.FailedAt)
	return tracked && idle
}
