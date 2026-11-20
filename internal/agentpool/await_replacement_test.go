package agentpool

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ENG-2266: AwaitReplacement waits until the worker a failed dispatch named can serve
// again — replaced by another instance, or the same one reporting idle by heartbeat
// after the failure — and never takes "still registered" or "not heartbeat-tracked"
// for ready: a worker whose transport broke is dead until something proves otherwise.
func TestAwaitReplacement_WaitsForAReplacementOrAFreshIdleHeartbeat(t *testing.T) {
	f := newFake()
	p := New(f, Config{AgentID: "a", Size: 1, RecoverPoll: 2 * time.Millisecond})
	fail := &WorkerFailure{Stream: "s", InstanceID: "inst-1", FailedAt: time.Now(), Err: errors.New("broken")}

	// Still registered as inst-1, untracked: not ready (bounded by ctx).
	f.set(func(f *fakeDispatcher) { f.live["s"] = true; f.instance["s"] = "inst-1" })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	if err := p.AwaitReplacement(ctx, fail); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a still-registered, untracked worker was taken as ready: %v", err)
	}
	cancel()

	// Deregistered, then replaced by inst-2: ready.
	f.set(func(f *fakeDispatcher) { f.live["s"] = false })
	go func() {
		time.Sleep(15 * time.Millisecond)
		f.set(func(f *fakeDispatcher) { f.live["s"] = true; f.instance["s"] = "inst-2" })
	}()
	start := time.Now()
	if err := p.AwaitReplacement(context.Background(), fail); err != nil || time.Since(start) < 10*time.Millisecond {
		t.Fatalf("replacement: err=%v after %s", err, time.Since(start))
	}

	// The same instance reporting idle by heartbeat after the failure: ready.
	f.set(func(f *fakeDispatcher) { f.instance["s"] = "inst-1"; f.tracked["s"] = true; f.idle["s"] = true })
	if err := p.AwaitReplacement(context.Background(), fail); err != nil {
		t.Fatalf("idle heartbeat: %v", err)
	}

	// An error that names no worker returns at once; the wrapper keeps the cause.
	if err := p.AwaitReplacement(context.Background(), errors.New("x")); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(&WorkerFailure{Err: ErrWorkerLost}, ErrWorkerLost) || fail.Error() != "broken" {
		t.Fatal("WorkerFailure must be transparent")
	}
}
