package harness

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/cambrian-sh/core/domain"
	"google.golang.org/grpc/status"
)

// StepFunc is the per-step execution callback used by SelfHealer.
// Defined locally to avoid a circular import with internal/substrate.
type StepFunc func(ctx context.Context, handoff *domain.Handoff) (*domain.Handoff, error)

// Restorer is satisfied by AgentManager (once issue #0005-06 lands) and by
// test mocks. SelfHealer depends only on this interface, never the concrete type.
type Restorer interface {
	Restore(agentID, taskID string) error
}

// HealingExhaustedError is returned when SelfHealer has consumed all retry
// attempts without a successful execution.
type HealingExhaustedError struct {
	StepIndex    int
	AttemptCount int
	LastError    error
	LoopDetected bool
}

func (e *HealingExhaustedError) Error() string {
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
type SelfHealer struct {
	Restorer    Restorer
	AgentID     string
	TaskID      string
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

			// Loop detection: only possible after the first attempt.
			if hasPrev && Detect(prevAttempt, currAttempt) {
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

			// Workspace rollback — best-effort.
			if sh.Restorer != nil {
				if restoreErr := sh.Restorer.Restore(sh.AgentID, sh.TaskID); restoreErr != nil {
					slog.Warn("self-healer: workspace restore failed",
						"step", sh.StepIndex,
						"attempt", attempt,
						"err", restoreErr,
					)
				}
			}

			prevAttempt = currAttempt
			hasPrev = true
		}

		return nil, &HealingExhaustedError{
			LoopDetected: false,
			AttemptCount: max,
			StepIndex:    sh.StepIndex,
			LastError:    lastErr,
		}
	}
}
