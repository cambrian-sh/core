package circadian

import (
	"context"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/cambrian-sh/core/domain"
)

// TestMain fails the package when any test leaves a goroutine behind
// (AGENTS.md §3.3). The tests below START the worker so there is something to
// verify; before 2026-10-31 this file held only an empty placeholder test.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

type countingEvictor struct{ n atomic.Int32 }

func (e *countingEvictor) EvictExpired() { e.n.Add(1) }

type countingSweeper struct{ n atomic.Int32 }

func (s *countingSweeper) SweepIdle(context.Context, time.Duration) (int, error) {
	s.n.Add(1)
	return 1, nil
}

type countingPurger struct{ n atomic.Int32 }

func (p *countingPurger) PurgeCompletedBefore(context.Context, time.Time) (int64, error) {
	p.n.Add(1)
	return 1, nil
}

func (p *countingPurger) PurgeOrphanRunsBefore(context.Context, time.Time) (int64, error) {
	return 0, nil
}

type noSessions struct{}

func (noSessions) ListSessions(context.Context, domain.SessionStatus) ([]domain.Session, error) {
	return nil, nil
}

func (noSessions) TransitionStatus(context.Context, domain.SessionID, domain.SessionStatus) error {
	return nil
}

// liveSweepGoroutines returns the stacks of any goroutine currently running a
// CircadianRhythm sweep, from ONE runtime.Stack snapshot with no retry, or "" if none.
func liveSweepGoroutines() string {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	var live []string
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "circadian.(*CircadianRhythm).start") {
			live = append(live, g)
		}
	}
	return strings.Join(live, "\n\n")
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// All three sweeps run under Start and every one of their goroutines exits on
// Stop. goleak.VerifyNone here checks the worker's own cleanup directly; the
// TestMain check above would otherwise attribute a leak to the package as a
// whole.
func TestCircadianRhythm_StartRunsAllSweeps_StopLeavesNoGoroutines(t *testing.T) {
	evictor := &countingEvictor{}
	sweeper := &countingSweeper{}
	purger := &countingPurger{}

	r := New(noSessions{}, 0)
	r.SessionEvictor = evictor
	r.SessionSweepInterval = time.Millisecond
	r.IdleSweeper = sweeper
	r.SessionIdleTimeout = time.Hour
	r.SessionIdleSweepInterval = time.Millisecond
	r.RetentionPurger = purger
	r.SessionRetention = time.Hour
	r.RetentionSweepInterval = time.Millisecond

	r.Start(context.Background())
	waitFor(t, "lease sweep tick", func() bool { return evictor.n.Load() > 0 })
	waitFor(t, "idle sweep tick", func() bool { return sweeper.n.Load() > 0 })
	waitFor(t, "retention sweep tick", func() bool { return purger.n.Load() > 0 })

	r.Stop()
	// Checked with a single, immediate stack snapshot rather than goleak.VerifyNone:
	// goleak retries until goroutines exit, so it masks a Stop that returns before
	// draining (the cause of the 2026-11-01 flake). Stop itself must drain.
	if live := liveSweepGoroutines(); live != "" {
		t.Fatalf("sweep goroutine still running after Stop returned:\n%s", live)
	}
	e, s, p := evictor.n.Load(), sweeper.n.Load(), purger.n.Load()
	r.Stop() // idempotent
	goleak.VerifyNone(t)
	time.Sleep(10 * time.Millisecond)
	if evictor.n.Load() != e || sweeper.n.Load() != s || purger.n.Load() != p {
		t.Fatal("a sweep ticked after Stop")
	}
}

// A sweep whose dependency or interval is unset must not start a goroutine at
// all, and Stop on such a rhythm is a no-op rather than a nil-func panic.
func TestCircadianRhythm_DisabledSweepsStartNothing(t *testing.T) {
	r := New(noSessions{}, 0) // no evictor, sweeper or purger; zero intervals
	r.Start(context.Background())
	goleak.VerifyNone(t)
	r.Stop()

	unstarted := New(noSessions{}, 0)
	unstarted.Stop() // never started: stop must be safe
}

// Cancelling the parent context stops the sweeps — the kernel's shutdown path,
// which cancels its root context and then calls Stop.
func TestCircadianRhythm_ParentCancelStopsSweeps(t *testing.T) {
	evictor := &countingEvictor{}
	r := New(noSessions{}, 0)
	r.SessionEvictor = evictor
	r.SessionSweepInterval = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	r.Start(ctx)
	waitFor(t, "lease sweep tick", func() bool { return evictor.n.Load() > 0 })
	cancel()
	// The kernel calls Stop after cancelling its root context (app.go); Stop must still
	// drain the goroutines that already observed the cancellation, and must not block.
	r.Stop()
	if live := liveSweepGoroutines(); live != "" {
		t.Fatalf("sweep goroutine still running after Stop returned:\n%s", live)
	}
	goleak.VerifyNone(t)
}
