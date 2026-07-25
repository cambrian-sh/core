package domain

import "time"

// CheckpointMeta describes a persisted execution checkpoint.
type CheckpointMeta struct {
	// RunID is the execution the checkpoint belongs to. Checkpoints are keyed by RUN, not
	// by session: a session accumulates many runs, and a step index only means anything
	// relative to the plan of the run it was taken against.
	RunID     RunID
	SessionID SessionID
	PlanID    string
	StepIndex int
	Timestamp time.Time
}

// CheckpointStore persists and restores mid-plan execution state.
//
// Keyed by (RunID, StepIndex). The previous key was (SessionID, PlanID, StepIndex) with the
// step rendered as an unpadded decimal, which produced two independent defects: listing
// returned checkpoints from EVERY plan in the session, and bbolt's lexicographic cursor
// ordered step "10" before step "2". "The latest checkpoint" was therefore neither the
// newest plan's nor the highest step's.
type CheckpointStore interface {
	SaveCheckpoint(runID RunID, stepIndex int, ctx map[string]string) error
	LoadCheckpoint(runID RunID, stepIndex int) (map[string]string, error)
	// ListCheckpoints returns the run's checkpoints in ascending step order.
	ListCheckpoints(runID RunID) ([]CheckpointMeta, error)
}
