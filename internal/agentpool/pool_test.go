package agentpool

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// fakeDispatcher records spawns/stops and lets a test control call behaviour.
type fakeDispatcher struct {
	mu sync.Mutex

	spawned  []string // stream ids, in spawn order (includes respawns)
	stopped  []string
	called   []string // stream ids that served a Dispatch
	live     map[string]bool
	spawnErr error
	callErr  error
	// block, when non-nil, holds CallDaemon until closed — used to saturate the pool.
	block chan struct{}
}

func newFake() *fakeDispatcher { return &fakeDispatcher{live: map[string]bool{}} }

func (f *fakeDispatcher) SpawnDaemon(agentID, streamID string, _ map[string]any) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.spawnErr != nil {
		return "", f.spawnErr
	}
	f.spawned = append(f.spawned, streamID)
	f.live[streamID] = true
	return "inst-" + streamID, nil
}

func (f *fakeDispatcher) StopDaemon(streamID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, streamID)
	delete(f.live, streamID)
	return nil
}

func (f *fakeDispatcher) DaemonInstanceID(streamID string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ok := f.live[streamID]
	return "inst-" + streamID, ok
}

func (f *fakeDispatcher) CallDaemon(ctx context.Context, streamID string, _ *domain.Handoff) (*domain.Handoff, error) {
	f.mu.Lock()
	f.called = append(f.called, streamID)
	blk, err := f.block, f.callErr
	f.mu.Unlock()

	if blk != nil {
		select {
		case <-blk:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	return &domain.Handoff{Payload: &domain.Payload{Data: []byte("ok")}}, nil
}

func (f *fakeDispatcher) counts() (spawned, stopped, called int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.spawned), len(f.stopped), len(f.called)
}

func testPool(t *testing.T, f *fakeDispatcher, cfg Config) *Pool {
	t.Helper()
	p := New(f, cfg)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(p.Stop)
	return p
}

func TestPool_StartSpawnsSizeWorkersAndStopStopsThem(t *testing.T) {
	f := newFake()
	p := New(f, Config{AgentID: "chat_session_agent", Size: 3})
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if spawned, _, _ := f.counts(); spawned != 3 {
		t.Fatalf("spawned %d workers, want 3", spawned)
	}
	p.Stop()
	if _, stopped, _ := f.counts(); stopped != 3 {
		t.Fatalf("stopped %d workers, want 3", stopped)
	}
}

// A partially-started pool would silently serve at reduced capacity, so Start must be
// all-or-nothing.
func TestPool_StartIsAllOrNothing(t *testing.T) {
	f := newFake()
	f.spawnErr = errors.New("boom")
	p := New(f, Config{AgentID: "a", Size: 3})
	if err := p.Start(context.Background()); err == nil {
		t.Fatal("expected Start to fail when a worker cannot spawn")
	}
	if _, err := p.Dispatch(context.Background(), &domain.Handoff{}); !errors.Is(err, ErrPoolNotStarted) {
		t.Fatalf("failed Start must leave the pool unstarted, got %v", err)
	}
}

func TestPool_DispatchBeforeStart(t *testing.T) {
	p := New(newFake(), Config{AgentID: "a", Size: 1})
	if _, err := p.Dispatch(context.Background(), &domain.Handoff{}); !errors.Is(err, ErrPoolNotStarted) {
		t.Fatalf("want ErrPoolNotStarted, got %v", err)
	}
}

// Any worker may serve any turn — over enough turns every worker is used.
func TestPool_DispatchLoadBalancesAcrossWorkers(t *testing.T) {
	f := newFake()
	p := testPool(t, f, Config{AgentID: "a", Size: 3})

	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = p.Dispatch(context.Background(), &domain.Handoff{}) }()
	}
	wg.Wait()

	f.mu.Lock()
	used := map[string]bool{}
	for _, s := range f.called {
		used[s] = true
	}
	f.mu.Unlock()
	if len(used) != 3 {
		t.Fatalf("only %d of 3 workers were used: %v", len(used), used)
	}
}

// The core backpressure property: beyond Size+QueueSize in flight, shed IMMEDIATELY rather
// than queue without bound.
func TestPool_ShedsWhenSaturated(t *testing.T) {
	f := newFake()
	f.block = make(chan struct{})
	p := testPool(t, f, Config{AgentID: "a", Size: 1, QueueSize: 1})

	// Occupy the single worker.
	go func() { _, _ = p.Dispatch(context.Background(), &domain.Handoff{}) }()
	waitFor(t, func() bool { _, _, called := f.counts(); return called >= 1 })

	// Occupy the one queue slot.
	queued := make(chan struct{})
	go func() { close(queued); _, _ = p.Dispatch(context.Background(), &domain.Handoff{}) }()
	<-queued
	time.Sleep(30 * time.Millisecond) // let the waiter reach the free-worker wait

	// Third turn exceeds Size+QueueSize: shed, and do not block.
	_, err := p.Dispatch(context.Background(), &domain.Handoff{})
	if !errors.Is(err, ErrPoolBusy) {
		t.Fatalf("want ErrPoolBusy once saturated, got %v", err)
	}
	close(f.block)
}

// An admitted turn that waits too long is shed rather than hanging forever.
func TestPool_AcquireTimeoutSheds(t *testing.T) {
	f := newFake()
	f.block = make(chan struct{})
	p := testPool(t, f, Config{AgentID: "a", Size: 1, QueueSize: 5, AcquireTimeout: 40 * time.Millisecond})

	go func() { _, _ = p.Dispatch(context.Background(), &domain.Handoff{}) }()
	waitFor(t, func() bool { _, _, called := f.counts(); return called >= 1 })

	_, err := p.Dispatch(context.Background(), &domain.Handoff{})
	if !errors.Is(err, ErrPoolBusy) {
		t.Fatalf("want ErrPoolBusy on acquire timeout, got %v", err)
	}
	close(f.block)
}

// A lost worker is respawned, and the caller is told the turn never ran — the pool must not
// silently retry, because the caller is the only party that knows whether that is safe.
func TestPool_WorkerLostRespawnsAndReportsPrecisely(t *testing.T) {
	f := newFake()
	p := testPool(t, f, Config{AgentID: "a", Size: 1})

	f.mu.Lock()
	f.callErr = errors.New("no daemon registered for stream")
	for k := range f.live {
		delete(f.live, k) // the worker is gone
	}
	f.mu.Unlock()

	_, err := p.Dispatch(context.Background(), &domain.Handoff{})
	if !errors.Is(err, ErrWorkerLost) {
		t.Fatalf("want ErrWorkerLost, got %v", err)
	}
	f.mu.Lock()
	respawned := len(f.spawned) // 1 initial + 1 respawn
	f.mu.Unlock()
	if respawned != 2 {
		t.Fatalf("pool should have respawned the lost worker; spawn count = %d, want 2", respawned)
	}
}

// An error from a worker that is still LIVE means the turn ran and failed. It must not be
// reported as ErrWorkerLost and must not trigger a respawn — that distinction is what keeps a
// side-effecting turn from being re-run.
func TestPool_ExecutionErrorIsNotWorkerLoss(t *testing.T) {
	f := newFake()
	p := testPool(t, f, Config{AgentID: "a", Size: 1})

	f.mu.Lock()
	f.callErr = errors.New("tool failed")
	f.mu.Unlock()

	_, err := p.Dispatch(context.Background(), &domain.Handoff{})
	if err == nil || errors.Is(err, ErrWorkerLost) {
		t.Fatalf("execution failure must surface as itself, got %v", err)
	}
	if spawned, _, _ := f.counts(); spawned != 1 {
		t.Fatalf("live worker must not be respawned; spawn count = %d, want 1", spawned)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}
