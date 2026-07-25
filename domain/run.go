package domain

import "time"

// RunStatus is the lifecycle state of a single plan execution.
type RunStatus string

const (
	// RunRunning is in flight.
	RunRunning RunStatus = "running"
	// RunCompleted finished all steps.
	RunCompleted RunStatus = "completed"
	// RunFailed stopped on an error (including a partial plan).
	RunFailed RunStatus = "failed"
)

// Run is ONE plan execution within a Session — the aggregate the kernel has always had at
// runtime (the unnamed `executionID`) but never persisted.
//
// It exists because resume was unsound without it. Checkpoints were keyed by
// (session, plan, step) and "resume" meant: take the last checkpoint found anywhere in the
// session, and apply its step index to a plan that had just been generated fresh for a NEW
// request. The index referred to a different plan's steps, so resuming silently skipped work.
// A Run fixes that by owning both halves — the plan AND the checkpoints taken against it —
// so a step index always has a plan to be an index INTO.
//
// Session : Run is 1:N. A session is the goal; a run is one attempt at it.
type Run struct {
	ID        RunID
	SessionID SessionID
	// PlanID is the executor's internal plan identifier for this run. Retained because
	// step results and traces are correlated by it.
	PlanID  string
	Subject string
	Status  RunStatus
	// Plan is the executed plan, persisted so a resume replays against the SAME steps it
	// checkpointed. ADR-0012 §3 specified this ("the associated ExecutionPlan (for
	// replay)") and it was never implemented — which is precisely why resume could not be
	// made sound.
	Plan      *ExecutionPlan
	StartedAt time.Time
	EndedAt   time.Time
}

// Resumable reports whether a run can be continued: it must carry the plan its checkpoints
// were taken against, and must not already be finished.
func (r Run) Resumable() bool {
	return r.Plan != nil && len(r.Plan.Steps) > 0 && r.Status == RunRunning
}

// RunStore persists runs. The kernel owns it so a restart can still answer "what was this
// session doing, and against which plan?".
type RunStore interface {
	SaveRun(run Run) error
	GetRun(runID RunID) (*Run, error)
	ListRunsForSession(sessionID SessionID) ([]Run, error)
}
