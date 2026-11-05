package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The 2026-11-05 E2E (E15): the plan ceiling expired while step 2 was being
// dispatched, both attempts failed at once with the same DeadlineExceeded and no
// output, and the step was reported as "loop detected". A spent ctx is a timeout,
// it is not retried into, and the message names the cause
// (T-20261105-slow-model-reliability-hardcoded-20-s).
func TestSelfHealer_ExpiredStepContextIsATimeoutNotALoop(t *testing.T) {
	t.Parallel()
	cause := fmt.Errorf("plan exceeded max duration 30m0s (execution.plan.plan_max_duration_ms): %w", context.DeadlineExceeded)
	ctx, cancel := context.WithTimeoutCause(context.Background(), time.Millisecond, cause)
	defer cancel()
	<-ctx.Done()

	calls := 0
	inner := func(ctx context.Context, _ *domain.Handoff) (*domain.Handoff, error) {
		calls++
		return nil, status.Error(codes.DeadlineExceeded, ctx.Err().Error())
	}
	_, err := (&SelfHealer{StepIndex: 2, MaxAttempts: 3}).Wrap(inner)(ctx, &domain.Handoff{})

	var ex *HealingExhaustedError
	if !errors.As(err, &ex) {
		t.Fatalf("want HealingExhaustedError, got %T: %v", err, err)
	}
	if ex.LoopDetected {
		t.Fatalf("a timeout was reported as a loop: %v", err)
	}
	if !ex.TimedOut {
		t.Fatalf("want TimedOut, got %+v", ex)
	}
	if calls != 1 {
		t.Fatalf("retried %d times into a dead ctx; want exactly 1 attempt", calls)
	}
	msg := err.Error()
	if !strings.Contains(msg, "timed out") || !strings.Contains(msg, "plan_max_duration_ms") {
		t.Fatalf("message should say timed out and name the plan ceiling, got %q", msg)
	}
	if strings.Contains(msg, "loop") {
		t.Fatalf("message mentions a loop: %q", msg)
	}
}

// The plan idle watchdog CANCELS the plan ctx (ctx.Err() is context.Canceled) with
// a cause wrapping DeadlineExceeded. That is a timeout, named by its cause — not a
// caller cancellation (T-20261105-owner-decision-needed-120-s-plan-timeout-ms).
func TestSelfHealer_IdleCancelledPlanIsATimeout(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(fmt.Errorf("plan idle for 3m0s, limit 3m0s (execution.plan.plan_idle_timeout_ms): %w", context.DeadlineExceeded))

	inner := func(ctx context.Context, _ *domain.Handoff) (*domain.Handoff, error) {
		return nil, status.Error(codes.Canceled, ctx.Err().Error())
	}
	_, err := (&SelfHealer{StepIndex: 1}).Wrap(inner)(ctx, &domain.Handoff{})
	var ex *HealingExhaustedError
	if !errors.As(err, &ex) || !ex.TimedOut || ex.LoopDetected {
		t.Fatalf("want a TimedOut HealingExhaustedError, got %T: %v", err, err)
	}
	if msg := err.Error(); !strings.Contains(msg, "timed out") || !strings.Contains(msg, "plan_idle_timeout_ms") {
		t.Fatalf("message should say timed out and name the idle key, got %q", msg)
	}

	// A plain caller cancellation is still NOT a timeout.
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	_, err = (&SelfHealer{StepIndex: 1}).Wrap(inner)(ctx2, &domain.Handoff{})
	if !errors.As(err, &ex) || ex.TimedOut {
		t.Fatalf("a caller cancellation must not read as a timeout: %v", err)
	}
}

// A step whose own deadline fires (parent ctx still alive) is retried — the next
// attempt may be faster — but two identical timeouts are still not a loop.
func TestSelfHealer_RepeatedStepTimeoutsAreNotALoop(t *testing.T) {
	t.Parallel()
	calls := 0
	inner := func(_ context.Context, _ *domain.Handoff) (*domain.Handoff, error) {
		calls++
		return nil, fmt.Errorf("dispatch: step timed out (agent a): %w", context.DeadlineExceeded)
	}
	_, err := (&SelfHealer{StepIndex: 1, MaxAttempts: 3}).Wrap(inner)(context.Background(), &domain.Handoff{})

	var ex *HealingExhaustedError
	if !errors.As(err, &ex) {
		t.Fatalf("want HealingExhaustedError, got %T: %v", err, err)
	}
	if ex.LoopDetected || !ex.TimedOut {
		t.Fatalf("want TimedOut and not LoopDetected, got %+v", ex)
	}
	if calls != 3 {
		t.Fatalf("want all 3 attempts for a live ctx, got %d", calls)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the chain must still unwrap to DeadlineExceeded: %v", err)
	}
}

// A wrapped context deadline is infrastructure, not the agent's logic error.
func TestClassify_WrappedContextDeadlineIsSystemError(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("dispatch: step timed out: %w", context.DeadlineExceeded)
	if got := Classify(err, codes.Unknown, ""); got != SystemError {
		t.Fatalf("want SystemError, got %v", got)
	}
}
