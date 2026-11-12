package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
)

// StepFailures counts a step's identical failed tool calls — same tool, same
// arguments, same error — and stops the step at the threshold
// (execution.tools.repeated_failure_stop; owner decision 2026-11-12 on
// T-20261112-yield-sub-goal-on-a-composed-lease-runs-under: "a step stops after
// 2-3 identical (tool, args, error) failures and that error goes to the
// re-planner"). The count is per STEP LEASE, kept by the kernel: a step's heal
// retries, its yielded sub-goals and their resumes all run on the step's lease, so
// none of them resets it (a per-agent-run counter in the SDK resets on each).
//
// One call before the threshold the failure is answered with a hint (the call
// will not succeed if repeated; change it or report the step cannot be done). At
// the threshold the step is marked stuck: the failure is answered with the stop
// notice, every later tool call on the lease is refused without running, and the
// dispatcher ends the step with the StepStuckError when its worker returns
// (dispatch.Dispatcher). The entry goes with the lease (ToolExecutor.EndRun).
//
// nil is off and safe to call.
type StepFailures struct {
	threshold int

	mu      sync.Mutex
	byLease map[LeaseID]*leaseFailures
}

type leaseFailures struct {
	counts map[string]int
	stuck  *StepStuckError
}

// NewStepFailures returns the tracker, or nil (off) for threshold < 2: one failed
// call is information, not a loop.
func NewStepFailures(threshold int) *StepFailures {
	if threshold < 2 {
		return nil
	}
	return &StepFailures{threshold: threshold, byLease: map[LeaseID]*leaseFailures{}}
}

// Threshold is the number of identical failures that stops a step (0 when off).
func (f *StepFailures) Threshold() int {
	if f == nil {
		return 0
	}
	return f.threshold
}

// RepeatedFailureHint is appended to a failure one short of the threshold.
const RepeatedFailureHint = " [This exact call (same tool, same arguments) has now failed %d times with the same error; " +
	"repeating it will not succeed. Change the arguments or the approach, or report that the step cannot be done " +
	"with the tools it has. One more identical failure stops the step.]"

// RepeatedFailureStop is appended to the failure that reaches the threshold, and
// is the refusal of every later call on the step.
const RepeatedFailureStop = " [The step is stopped: this exact call failed %d times with the same error. " +
	"No further tool call runs in this step; it goes back to the planner with this error. Answer with what you have.]"

// Observe records one failed call (errText is the error or the refusal reason)
// and returns how many times this identical failure has now happened and whether
// the step is stuck. An empty lease is not tracked.
func (f *StepFailures) Observe(lease LeaseID, tool string, args []byte, errText string) (count int, stuck bool) {
	if f == nil || lease == "" {
		return 0, false
	}
	key := failureKey(tool, args, errText)
	f.mu.Lock()
	defer f.mu.Unlock()
	lf := f.byLease[lease]
	if lf == nil {
		lf = &leaseFailures{counts: map[string]int{}}
		f.byLease[lease] = lf
	}
	lf.counts[key]++
	count = lf.counts[key]
	if count >= f.threshold && lf.stuck == nil {
		lf.stuck = &StepStuckError{Tool: tool, ArgsPreview: preview(args, 200), Err: errText, Count: count}
	}
	return count, lf.stuck != nil
}

// Stuck returns the error that stopped the step on lease, or nil.
func (f *StepFailures) Stuck(lease LeaseID) *StepStuckError {
	if f == nil || lease == "" {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if lf := f.byLease[lease]; lf != nil {
		return lf.stuck
	}
	return nil
}

// End forgets the lease (its step is over).
func (f *StepFailures) End(lease LeaseID) {
	if f == nil || lease == "" {
		return
	}
	f.mu.Lock()
	delete(f.byLease, lease)
	f.mu.Unlock()
}

// failureKey identifies one failure: tool, canonical arguments (sorted keys, so
// two encodings of the same call match) and the error text.
func failureKey(tool string, args []byte, errText string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s", tool, CanonicalJSON(args), errText)
	return hex.EncodeToString(h.Sum(nil))
}
