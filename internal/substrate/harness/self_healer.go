package harness

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/cambrian-sh/core/domain"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// StepFunc is the per-step execution callback used by SelfHealer.
// Defined locally to avoid a circular import with internal/substrate.
type StepFunc func(ctx context.Context, handoff *domain.Handoff) (*domain.Handoff, error)

// HealingExhaustedError is returned when SelfHealer has consumed all retry
// attempts without a successful execution.
type HealingExhaustedError struct {
	StepIndex    int
	AttemptCount int
	LastError    error
	LoopDetected bool
	// TimedOut reports that the step failed by running out of time — its own
	// per-step deadline, or the step's ctx (the plan's idle watchdog or max-duration
	// cap) ending — rather
	// than by repeating itself. A timeout is never reported as a loop: two
	// attempts that both time out return the same error and no output, which is
	// exactly what loop detection matches, and "loop detected" then sends the
	// operator looking for a reasoning loop that does not exist
	// (T-20261105-slow-model-reliability-hardcoded-20-s).
	TimedOut bool
	// Cause is context.Cause of the step's ctx when that ctx ended (e.g. "plan idle
	// for 3m0s, limit 3m0s (execution.plan.plan_idle_timeout_ms)" or "plan exceeded
	// max duration 30m0s (execution.plan.plan_max_duration_ms)"); nil while it is
	// alive.
	Cause error
}

func (e *HealingExhaustedError) Error() string {
	if e.TimedOut {
		msg := fmt.Sprintf("self-healer: step %d timed out after %d attempt(s): %v",
			e.StepIndex, e.AttemptCount, e.LastError)
		// A bare context error adds nothing to the message; a named cause does.
		if e.Cause != nil && e.Cause != context.DeadlineExceeded && e.Cause != context.Canceled {
			msg += " (" + e.Cause.Error() + ")"
		}
		return msg
	}
	if e.LoopDetected {
		return fmt.Sprintf(
			"self-healer: loop detected at step %d after %d attempt(s): %v",
			e.StepIndex, e.AttemptCount, e.LastError,
		)
	}
	return fmt.Sprintf(
		"self-healer: healing exhausted at step %d after %d attempt(s): %v",
		e.StepIndex, e.AttemptCount, e.LastError,
	)
}

// Unwrap returns the underlying last error so errors.As/errors.Is can unwrap it.
func (e *HealingExhaustedError) Unwrap() error { return e.LastError }

// SelfHealer wraps a StepFunc with a retry loop, fault classification, and
// loop detection. MaxAttempts defaults to 3 if zero.
//
// It does NOT roll the agent's workspace back between attempts. ADR-0005 D2b/D3
// (git snapshot + restore) was removed by owner decision 2026-10-31: the
// snapshot ran `git add -A && git commit` in the agent dir, which committed the
// operator's enclosing repo, and no snapshot was ever taken on the path plan
// steps use (agentplane.Transport), so the restore here never ran. A failed
// attempt's file changes are therefore visible to the next attempt.
type SelfHealer struct {
	StepIndex   int
	MaxAttempts int
}

// maxAttempts returns MaxAttempts if set, otherwise the default of 3.
func (sh *SelfHealer) maxAttempts() int {
	if sh.MaxAttempts > 0 {
		return sh.MaxAttempts
	}
	return 3
}

// Wrap returns a new StepFunc that retries inner up to MaxAttempts times,
// injecting healing context on LogicErrors and detecting retry loops.
func (sh *SelfHealer) Wrap(inner StepFunc) StepFunc {
	return func(ctx context.Context, handoff *domain.Handoff) (*domain.Handoff, error) {
		max := sh.maxAttempts()
		var prevAttempt Attempt
		hasPrev := false
		var lastErr error

		for attempt := 0; attempt < max; attempt++ {
			resp, err := inner(ctx, handoff)
			if err == nil {
				return resp, nil
			}
			lastErr = err

			// Extract gRPC status code and agent-reported error type.
			st, _ := status.FromError(err)
			agentErrorType := ""
			if resp != nil && resp.Context != nil {
				agentErrorType = resp.Context["_error_type"]
			}

			// Build current attempt record for loop detection.
			currAttempt := Attempt{
				ErrorMsg: err.Error(),
			}
			if resp != nil && resp.Payload != nil {
				currAttempt.Output = resp.Payload.Data
			}

			// The step's own ctx is over (the plan ceiling fired, or the caller
			// gave up): every further attempt would fail at once on the same dead
			// ctx, so stop here and say why instead of retrying into it. The plan
			// idle watchdog CANCELS the plan ctx (ctx.Err() is Canceled) with a
			// cause that wraps DeadlineExceeded, so the cause decides "timed out"
			// (T-20261105-owner-decision-needed-120-s-plan-timeout-ms).
			if cerr := ctx.Err(); cerr != nil {
				return nil, &HealingExhaustedError{
					TimedOut:     errors.Is(cerr, context.DeadlineExceeded) || errors.Is(context.Cause(ctx), context.DeadlineExceeded),
					AttemptCount: attempt + 1,
					StepIndex:    sh.StepIndex,
					LastError:    err,
					Cause:        context.Cause(ctx),
				}
			}

			// A deterministic dispatch refusal (the effect check found no agent whose
			// tools can do the step) is the same answer on every attempt: stop now so
			// the plan replans with the reason, instead of re-asking the gatekeeper.
			if domain.IsDispatchRefusal(err) {
				return nil, &HealingExhaustedError{
					AttemptCount: attempt + 1,
					StepIndex:    sh.StepIndex,
					LastError:    err,
				}
			}

			timedOut := isTimeout(err, st.Code())

			// Loop detection: only possible after the first attempt, and never for
			// a timeout — repeated timeouts look identical (same error, no output)
			// without the agent having repeated anything.
			if !timedOut && hasPrev && Detect(prevAttempt, currAttempt) {
				return nil, &HealingExhaustedError{
					LoopDetected: true,
					AttemptCount: attempt + 1,
					StepIndex:    sh.StepIndex,
					LastError:    err,
				}
			}

			// Classify the fault.
			class := Classify(err, st.Code(), agentErrorType)

			// Mutate handoff context based on fault class.
			if class == LogicError {
				if handoff.Context == nil {
					handoff.Context = make(map[string]string)
				}
				handoff.Context["_heal_error"] = err.Error()
				handoff.Context["_heal_attempt"] = strconv.Itoa(attempt + 1)
			}
			// For SystemError: leave handoff unchanged.

			prevAttempt = currAttempt
			hasPrev = true
		}

		st, _ := status.FromError(lastErr)
		return nil, &HealingExhaustedError{
			LoopDetected: false,
			TimedOut:     isTimeout(lastErr, st.Code()),
			AttemptCount: max,
			StepIndex:    sh.StepIndex,
			LastError:    lastErr,
		}
	}
}

// isTimeout reports whether a step error is a deadline: a gRPC DeadlineExceeded
// from the agent call, or a context deadline anywhere in the chain (the
// dispatcher's "step timed out" wraps context.DeadlineExceeded).
func isTimeout(err error, code codes.Code) bool {
	return code == codes.DeadlineExceeded || errors.Is(err, context.DeadlineExceeded)
}
