package domain

import "time"

// SessionEventType categorises entries in a Session's narrative log.
type SessionEventType string

const (
	EventUserMessage      SessionEventType = "user_message"
	EventSystemThought    SessionEventType = "system_thought"
	EventHITLIntervention SessionEventType = "hitl_intervention"
	EventCriticalError    SessionEventType = "critical_error"
	EventBudgetBreach     SessionEventType = "budget_breach"
	EventCheckpointSaved  SessionEventType = "checkpoint_saved"
)

// SessionEvent is a timestamped entry in a Session's narrative log.
// Fed into Planner as "Social Context" for mood-aware prompt generation.
// MemoryAgent observes and ingests significant events to LTM. See ADR-0012.
type SessionEvent struct {
	SessionID   string           `json:"session_id"`
	Type        SessionEventType `json:"type"`
	Timestamp   time.Time        `json:"timestamp"`
	Payload     string           `json:"payload"`
	ArtifactIDs []string         `json:"artifact_ids,omitempty"`
}
