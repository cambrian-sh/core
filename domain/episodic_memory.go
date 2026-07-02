package domain

import "time"

// EpisodicMemory is the session narrative index produced by ConsolidatorAgent on session
// completion. It captures what was decided and learned during a session and is stored as
// DocTypeEpisodicMemory in pgvector for cross-session retrieval. ADR-0029.
//
// Outcome is intentionally absent — sessions are conversations, not transactions.
type EpisodicMemory struct {
	SessionID    string       `json:"session_id"`
	StartedAt    time.Time    `json:"started_at"`
	CompletedAt  time.Time    `json:"completed_at"`
	Goal         string       `json:"goal"`
	Decisions    []Decision   `json:"decisions"`
	ActionItems  []ActionItem `json:"action_items"`
	Participants []string     `json:"participants"` // agent IDs from TaskEvent records
	KeyFacts     []string     `json:"key_facts"`    // doc IDs: referenced + created during session
}

// Decision is a single decision extracted from a SessionEvent by ConsolidatorAgent. ADR-0029.
type Decision struct {
	Text            string           `json:"text"`
	MadeAt          time.Time        `json:"made_at"`
	SourceEventType SessionEventType `json:"source_event_type"`
}

// ActionItem is a follow-up task extracted from a session by ConsolidatorAgent. ADR-0029.
type ActionItem struct {
	Text string `json:"text"`
}
