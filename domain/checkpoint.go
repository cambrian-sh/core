package domain

import "time"

// CheckpointMeta describes a persisted execution checkpoint.
type CheckpointMeta struct {
	SessionID string
	PlanID    string
	StepIndex int
	Timestamp time.Time
}

// CheckpointStore persists and restores mid-plan execution state.
type CheckpointStore interface {
	SaveCheckpoint(sessionID, planID string, stepIndex int, ctx map[string]string) error
	LoadCheckpoint(sessionID, planID string, stepIndex int) (map[string]string, error)
	ListCheckpoints(sessionID string) ([]CheckpointMeta, error)
}
