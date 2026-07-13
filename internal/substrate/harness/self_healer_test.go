package harness

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// mockRestorer counts Restore calls and can return a configured error.
type mockRestorer struct {
	restoreCalls int
	err          error
}

func (m *mockRestorer) Restore(agentID, taskID string) error {
	m.restoreCalls++
	return m.err
}

// TestSelfHealer_FirstPassSuccess: inner returns success on first call.
// Wrap must return the result immediately; Restorer.Restore must never be called.
func TestSelfHealer_FirstPassSuccess(t *testing.T) {
	restorer := &mockRestorer{}
	sh := &SelfHealer{
		Restorer:    restorer,
		AgentID:     "agent-1",
		TaskID:      "task-1",
		StepIndex:   0,
		MaxAttempts: 3,
	}

	want := &domain.Handoff{
		Payload: &domain.Payload{Data: []byte("ok")},
	}
	inner := func(_ context.Context, _ *domain.Handoff) (*domain.Handoff, error) {
		return want, nil
	}

	wrapped := sh.Wrap(inner)
	got, err := wrapped(context.Background(), &domain.Handoff{})

	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if got != want {
		t.Fatalf("expected returned handoff to be the same pointer")
	}
	if restorer.restoreCalls != 0 {
		t.Fatalf("expected Restore to never be called, got %d calls", restorer.restoreCalls)
	}
}

// TestSelfHealer_LogicError_ContextInjected: inner fails once (LogicError), then
// succeeds. On the retry call, handoff.Context must contain _heal_error and
// _heal_attempt. The result from the second call is returned.
func TestSelfHealer_LogicError_ContextInjected(t *testing.T) {
	restorer := &mockRestorer{}
	sh := &SelfHealer{
		Restorer:    restorer,
		AgentID:     "agent-1",
		TaskID:      "task-1",
		StepIndex:   0,
		MaxAttempts: 3,
	}

	logicErr := errors.New("bad logic") // plain error → LogicError by Classify

	callCount := 0
	var healErrOnRetry, healAttemptOnRetry string
	want := &domain.Handoff{Payload: &domain.Payload{Data: []byte("recovered")}}

	inner := func(_ context.Context, h *domain.Handoff) (*domain.Handoff, error) {
		callCount++
		if callCount == 1 {
			return nil, logicErr
		}
		// Capture what was injected into handoff.Context on the retry.
		healErrOnRetry = h.Context["_heal_error"]
		healAttemptOnRetry = h.Context["_heal_attempt"]
		return want, nil
	}

	got, err := sh.Wrap(inner)(context.Background(), &domain.Handoff{})

	if err != nil {
		t.Fatalf("expected no error on second attempt, got %v", err)
	}
	if got != want {
		t.Fatalf("expected result from second call")
	}
	if healErrOnRetry != logicErr.Error() {
		t.Fatalf("_heal_error: want %q, got %q", logicErr.Error(), healErrOnRetry)
	}
	if healAttemptOnRetry != "1" {
		t.Fatalf("_heal_attempt: want \"1\", got %q", healAttemptOnRetry)
	}
}

// TestSelfHealer_SystemError_HandoffUnchanged: inner fails once with a gRPC
// DeadlineExceeded error (SystemError), then succeeds. The handoff.Context must
// NOT be mutated with _heal_error or _heal_attempt on the retry.
func TestSelfHealer_SystemError_HandoffUnchanged(t *testing.T) {
	restorer := &mockRestorer{}
	sh := &SelfHealer{
		Restorer:    restorer,
		AgentID:     "agent-1",
		TaskID:      "task-1",
		StepIndex:   0,
		MaxAttempts: 3,
	}

	sysErr := status.Error(codes.DeadlineExceeded, "timeout")

	callCount := 0
	var healErrOnRetry, healAttemptOnRetry string
	want := &domain.Handoff{Payload: &domain.Payload{Data: []byte("ok after timeout")}}

	inner := func(_ context.Context, h *domain.Handoff) (*domain.Handoff, error) {
		callCount++
		if callCount == 1 {
			return nil, sysErr
		}
		healErrOnRetry = h.Context["_heal_error"]
		healAttemptOnRetry = h.Context["_heal_attempt"]
		return want, nil
	}

	got, err := sh.Wrap(inner)(context.Background(), &domain.Handoff{})

	if err != nil {
		t.Fatalf("expected no error on second attempt, got %v", err)
	}
	if got != want {
		t.Fatalf("expected result from second call")
	}
	if healErrOnRetry != "" {
		t.Fatalf("expected _heal_error to be empty for SystemError, got %q", healErrOnRetry)
	}
	if healAttemptOnRetry != "" {
		t.Fatalf("expected _heal_attempt to be empty for SystemError, got %q", healAttemptOnRetry)
	}
}

// TestSelfHealer_LoopDetected_EarlyReturn: inner always fails with the same
// error message and same output bytes, which makes Detect return true on the
// second attempt. Wrap must return HealingExhaustedError{LoopDetected: true,
// AttemptCount: 2} and call inner exactly twice.
func TestSelfHealer_LoopDetected_EarlyReturn(t *testing.T) {
	restorer := &mockRestorer{}
	sh := &SelfHealer{
		Restorer:    restorer,
		AgentID:     "agent-1",
		TaskID:      "task-1",
		StepIndex:   2,
		MaxAttempts: 3,
	}

	loopErr := errors.New("same error every time")
	loopOutput := []byte("same output every time")

	callCount := 0
	inner := func(_ context.Context, _ *domain.Handoff) (*domain.Handoff, error) {
		callCount++
		return &domain.Handoff{Payload: &domain.Payload{Data: loopOutput}}, loopErr
	}

	_, err := sh.Wrap(inner)(context.Background(), &domain.Handoff{})

	var exhausted *HealingExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("expected HealingExhaustedError, got %T: %v", err, err)
	}
	if !exhausted.LoopDetected {
		t.Fatalf("expected LoopDetected=true")
	}
	if exhausted.AttemptCount != 2 {
		t.Fatalf("expected AttemptCount=2, got %d", exhausted.AttemptCount)
	}
	if callCount != 2 {
		t.Fatalf("expected inner to be called 2 times, got %d", callCount)
	}
	if exhausted.StepIndex != 2 {
		t.Fatalf("expected StepIndex=2, got %d", exhausted.StepIndex)
	}
}

// TestSelfHealer_ThreeAttempts_Exhausted: inner always fails; outputs differ
// each call to avoid loop detection triggering early. Wrap must return
// HealingExhaustedError{LoopDetected: false, AttemptCount: 3}. Restorer.Restore
// must be called exactly 3 times.
func TestSelfHealer_ThreeAttempts_Exhausted(t *testing.T) {
	restorer := &mockRestorer{}
	sh := &SelfHealer{
		Restorer:    restorer,
		AgentID:     "agent-1",
		TaskID:      "task-1",
		StepIndex:   1,
		MaxAttempts: 3,
	}

	// Each call returns a different output so Detect never fires.
	outputs := [][]byte{
		[]byte("output-A"),
		[]byte("output-B-completely-different"),
		[]byte("output-C-also-very-different"),
	}
	callCount := 0
	finalErr := errors.New("persistent failure")

	inner := func(_ context.Context, _ *domain.Handoff) (*domain.Handoff, error) {
		out := outputs[callCount]
		callCount++
		return &domain.Handoff{Payload: &domain.Payload{Data: out}}, finalErr
	}

	_, err := sh.Wrap(inner)(context.Background(), &domain.Handoff{})

	var exhausted *HealingExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("expected HealingExhaustedError, got %T: %v", err, err)
	}
	if exhausted.LoopDetected {
		t.Fatalf("expected LoopDetected=false")
	}
	if exhausted.AttemptCount != 3 {
		t.Fatalf("expected AttemptCount=3, got %d", exhausted.AttemptCount)
	}
	if !errors.Is(err, finalErr) {
		t.Fatalf("expected LastError to unwrap to finalErr")
	}
	if restorer.restoreCalls != 3 {
		t.Fatalf("expected Restore called 3 times, got %d", restorer.restoreCalls)
	}
}

// TestHealingExhaustedError_ImplementsError: verifies the error type satisfies
// the error interface, works with errors.As, and Unwrap() returns LastError.
func TestHealingExhaustedError_ImplementsError(t *testing.T) {
	sentinel := errors.New("sentinel")
	he := &HealingExhaustedError{
		StepIndex:    3,
		AttemptCount: 3,
		LastError:    sentinel,
		LoopDetected: false,
	}

	// Error() must return a non-empty string.
	msg := he.Error()
	if msg == "" {
		t.Fatal("Error() returned empty string")
	}

	// errors.As must succeed when wrapping as a plain error.
	var wrapped error = he
	var target *HealingExhaustedError
	if !errors.As(wrapped, &target) {
		t.Fatal("errors.As failed for HealingExhaustedError")
	}

	// Unwrap() must return the original LastError.
	if !errors.Is(he, sentinel) {
		t.Fatal("errors.Is(he, sentinel) failed — Unwrap() not returning LastError")
	}
}
