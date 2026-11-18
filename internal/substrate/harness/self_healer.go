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
	// Class is the failure's step-recovery class (ClassifyStepFailure). A
	// deterministic or needs_replan failure ends the healing at once; the caller
	// sends a needs_replan one straight to the re-planner (no runner-up fallback).
	Class domain.StepFailureClass
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
	switch e.Class {
	case domain.StepFailureNeedsReplan:
		return fmt.Sprintf("self-healer: step %d needs re-planning (not retried) after %d attempt(s): %v",
			e.StepIndex, e.AttemptCount, e.LastError)
	case domain.StepFailureDeterministic:
		return fmt.Sprintf("self-healer: step %d failed deterministically (not retried) after %d attempt(s): %v",
			e.StepIndex, e.AttemptCount, e.LastError)
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

// ClassifyStepFailure is the step-recovery class of a step failure (owner decision
// 2026-11-12, T-20261112-self-healer-retries-a-step-whose-yield-chain; research note
// 2026-11-12-agent-harness-recovery-practices §2):
//
//   - the class the error itself carries (domain.ClassOfStepError): yield limits
//     and repeated intent are deterministic, a missing capability and a repeated
//     identical tool failure need re-planning, a dispatch refusal is
//     deterministic, a context deadline is transient;
//   - else the transport's view: Unavailable, ResourceExhausted (rate limit or
//     overload), Aborted and DeadlineExceeded are transient; InvalidArgument,
//     OutOfRange (a prompt over the context window, domain.ErrContextOverflow),
//     FailedPrecondition, PermissionDenied, Unauthenticated (a refused provider
//     credential, domain.ErrProviderAuth), NotFound and
//     Unimplemented are deterministic (validation and refusals: the same request
//     is refused again);
//   - an agent that reports _error_type "system" is transient;
//   - anything else is unclassified.
func ClassifyStepFailure(err error, code codes.Code, agentErrorType string) domain.StepFailureClass {
	if c := domain.ClassOfStepError(err); c != domain.StepFailureUnclassified {
		return c
	}
	// A prompt over the model's context window is the same prompt on the next
	// attempt: in process it is the sentinel, over the wire codes.OutOfRange
	// (gatewayStatus; the SDK's TraitServicer answers a worker that died of it
	// with OUT_OF_RANGE). p06 re-ran a 12-round step into the same overflow
	// (T-20261112-worker-resolve-cid-inline-has-no-size-cap-a).
	// A refused provider credential (domain.ErrProviderAuth; over the wire
	// codes.Unauthenticated, below) is refused again on every attempt until a
	// person fixes it: never re-run the step (owner decision 2026-11-18,
	// T-20261112-a-provider-auth-failure-streamed-through-the).
	if errors.Is(err, domain.ErrContextOverflow) || errors.Is(err, domain.ErrProviderAuth) {
		return domain.StepFailureDeterministic
	}
	switch code {
	case codes.Unavailable, codes.ResourceExhausted, codes.Aborted, codes.DeadlineExceeded:
		return domain.StepFailureTransient
	case codes.InvalidArgument, codes.OutOfRange, codes.FailedPrecondition, codes.PermissionDenied,
		codes.Unauthenticated, codes.NotFound, codes.Unimplemented:
		return domain.StepFailureDeterministic
	}
	if agentErrorType == "system" {
		return domain.StepFailureTransient
	}
	return domain.StepFailureUnclassified
}

// Wrap returns a new StepFunc that retries inner up to MaxAttempts times,
// injecting healing context on LogicErrors and detecting retry loops. Only a
// transient or unclassified failure is retried (ClassifyStepFailure): a
// deterministic one and one that needs re-planning end the healing at once,
// with the class on the HealingExhaustedError.
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

			// A deterministic failure (a dispatch refusal, a yield budget or depth
			// limit, a repeated intent, a refused or invalid request) is the same
			// answer on every attempt, and one that needs re-planning (a missing
			// capability, a repeated identical tool failure) cannot be fixed by the
			// same worker: stop now so the plan replans with the reason, instead of
			// running the step again.
			fc := ClassifyStepFailure(err, st.Code(), agentErrorType)
			if fc == domain.StepFailureDeterministic || fc == domain.StepFailureNeedsReplan {
				return nil, &HealingExhaustedError{
					AttemptCount: attempt + 1,
					StepIndex:    sh.StepIndex,
					LastError:    err,
					Class:        fc,
				}
			}

			timedOut := isTimeout(err, st.Code())

			// Loop detection: only possible after the first attempt, and never for
			// a timeout or another transient failure — repeated timeouts or an
			// unavailable agent look identical (same error, no output) without the
			// agent having repeated anything.
			if !timedOut && fc != domain.StepFailureTransient && hasPrev && Detect(prevAttempt, currAttempt) {
				return nil, &HealingExhaustedError{
					LoopDetected: true,
					AttemptCount: attempt + 1,
					StepIndex:    sh.StepIndex,
					LastError:    err,
				}
			}

			// Classify the fault.
			class := Classify(err, st.Code(), agentErrorType)

			// Mutate handoff context based on fault class. A transient failure is
			// retried with the same input: nothing about it is the agent's to fix.
			if class == LogicError && fc != domain.StepFailureTransient {
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
			Class:        ClassifyStepFailure(lastErr, st.Code(), ""),
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
