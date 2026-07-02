package domain

import "time"

// SessionStatus represents the lifecycle state of a Session.
type SessionStatus string

const (
	SessionActive    SessionStatus = "active"
	SessionPaused    SessionStatus = "paused"
	SessionDormant   SessionStatus = "dormant"
	SessionCompleted SessionStatus = "completed"
)

// Session is a persistent conversation container (UUID). Each Session
// holds multiple plan executions over time. Checkpoints are keyed by
// SessionID:PlanID:StepIndex. See ADR-0012.
type Session struct {
	ID          string        `json:"id"`
	ParentID    string        `json:"parent_id,omitempty"`
	Goal        string        `json:"goal"`
	Status      SessionStatus `json:"status"`
	Summary     string        `json:"summary,omitempty"`
	CreatedAt   time.Time     `json:"created_at"`
	UpdatedAt   time.Time     `json:"updated_at"`
	CompletedAt time.Time     `json:"completed_at,omitempty"`
	// CallerScope is the per-conversation caller_scope supplied by the integrating
	// application at StartConversation/dispatch and persisted SERVER-SIDE. The
	// Substrate re-derives effective = caller_scope ∩ agent_scope per RPC from this
	// field (looked up via session token) — NEVER from the forgeable Handoff.Context.
	// This is the non-forgeable transport that unlocks Phase-2 caller-scoped
	// enforcement. ADR-0034 (D13/R2).
	CallerScope ScopeConfig   `json:"caller_scope,omitempty"`
}
