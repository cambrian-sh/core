package clusterer

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// countingSource tracks how many times GetAllAgentEmbeddings is called.
// It returns enough agents to pass the minAgents guard so sweep logic runs.
type countingSource struct {
	callCount int32
	agents    []AgentEmbedding
	blockFor  time.Duration // if > 0, each call sleeps this long
}

func (s *countingSource) GetAllAgentEmbeddings(_ context.Context) ([]AgentEmbedding, error) {
	atomic.AddInt32(&s.callCount, 1)
	if s.blockFor > 0 {
		time.Sleep(s.blockFor)
	}
	return s.agents, nil
}

// ── Cycle 1: TriggerSweep is non-blocking ────────────────────────────────────

// TestTriggerSweep_NonBlocking verifies that TriggerSweep never blocks regardless
// of how many times it is called before a sweep drains the channel.
func TestTriggerSweep_NonBlocking(t *testing.T) {
	c := New(&mockSource{}, newMockStore(), &mockGenerator{}, 0.80, 0.02, 10)

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.TriggerSweep()
		c.TriggerSweep() // second call: channel full, must not block
		c.TriggerSweep() // third call: still full, must not block
	}()

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("TriggerSweep blocked — expected non-blocking send")
	}
}

// ── Cycle 2: runLoop drains triggerChan and calls runSweep ───────────────────

// TestRunLoop_TriggerFires_RunsSweep verifies that when a signal arrives on
// triggerChan, runLoop calls runSweep (evidenced by GetAllAgentEmbeddings call).
func TestRunLoop_TriggerFires_RunsSweep(t *testing.T) {
	src := &countingSource{}
	c := New(src, newMockStore(), &mockGenerator{}, 0.80, 0.02, 10)

	ctx, cancel := context.WithCancel(context.Background())
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		c.runLoop(ctx, nil) // nil tickC — no ticker track
	}()

	c.TriggerSweep()

	// Give the loop time to process the trigger.
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-loopDone

	if atomic.LoadInt32(&src.callCount) == 0 {
		t.Error("expected runSweep to be called after TriggerSweep, but callCount == 0")
	}
}

// ── Cycle 3: runLoop drains tickC and calls runSweep ─────────────────────────

// TestRunLoop_TickerFires_RunsSweep verifies the defensive reconciliation track:
// when tickC receives a tick, runLoop calls runSweep without any TriggerSweep call.
func TestRunLoop_TickerFires_RunsSweep(t *testing.T) {
	src := &countingSource{}
	c := New(src, newMockStore(), &mockGenerator{}, 0.80, 0.02, 10)

	tickCh := make(chan time.Time, 1)
	ctx, cancel := context.WithCancel(context.Background())
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		c.runLoop(ctx, tickCh)
	}()

	tickCh <- time.Now() // synthetic clock advance

	time.Sleep(20 * time.Millisecond)
	cancel()
	<-loopDone

	if atomic.LoadInt32(&src.callCount) == 0 {
		t.Error("expected runSweep to be called on ticker fire, but callCount == 0")
	}
}

// ── Cycle 4: Start returns nil on context cancellation ───────────────────────

// TestStart_CancelledContext_ReturnsNil verifies clean shutdown: Start returns
// nil (not an error) when ctx is cancelled and no goroutines are leaked.
func TestStart_CancelledContext_ReturnsNil(t *testing.T) {
	c := New(&mockSource{}, newMockStore(), &mockGenerator{}, 0.80, 0.02, 10)
	c.IntervalSeconds = 3600 // large interval so ticker doesn't fire during the test

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- c.Start(ctx)
	}()

	cancel()

	select {
	case err := <-result:
		if err != nil {
			t.Errorf("Start returned non-nil error on clean shutdown: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Start did not return after context cancellation (possible goroutine leak)")
	}
}

// ── Cycle 5: Debounce — 50 concurrent triggers → at most 2 sweeps ────────────

// TestTriggerSweep_Debounce_AtMostTwoSweeps verifies the channel-of-size-1 acts
// as a natural debounce: 50 concurrent TriggerSweep calls while one sweep is
// in-flight result in at most 2 completed sweeps (1 in-flight + 1 queued).
func TestTriggerSweep_Debounce_AtMostTwoSweeps(t *testing.T) {
	// Each sweep blocks for 30ms — long enough for all 50 triggers to pile up.
	src := &countingSource{blockFor: 30 * time.Millisecond}
	c := New(src, newMockStore(), &mockGenerator{}, 0.80, 0.02, 10)

	ctx, cancel := context.WithCancel(context.Background())
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		c.runLoop(ctx, nil)
	}()

	// Fire the first trigger to start sweep #1.
	c.TriggerSweep()
	time.Sleep(5 * time.Millisecond) // let sweep #1 start blocking

	// Fire 50 concurrent triggers while sweep #1 is in-flight.
	for i := 0; i < 50; i++ {
		go c.TriggerSweep()
	}

	// Wait long enough for at most 2 sweeps to complete (2 × 30ms + margin).
	time.Sleep(150 * time.Millisecond)
	cancel()
	<-loopDone

	got := int(atomic.LoadInt32(&src.callCount))
	if got > 2 {
		t.Errorf("debounce failed: expected at most 2 sweeps from 50 triggers, got %d", got)
	}
	if got == 0 {
		t.Error("expected at least 1 sweep to run, got 0")
	}
}
