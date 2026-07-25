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

// Session is a persistent TASK container (UUID): goal-scoped work that holds multiple plan
// executions over time and completes. Checkpoints are keyed by SessionID:PlanID:StepIndex.
// See ADR-0012.
//
// It is NOT a chat conversation — that is domain.Conversation (ADR-0084), a long-lived,
// message-ordered, owned exchange. A conversational turn that orders work references a
// Session; it is not itself one. (The doc previously called this a "conversation container",
// which predated the Conversation model and caused exactly the overload ADR-0084 untangles.)
type Session struct {
	ID          SessionID     `json:"id"`
	ParentID    SessionID     `json:"parent_id,omitempty"`
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
	CallerScope ScopeConfig `json:"caller_scope,omitempty"`

	// ConversationID is the exchange that ORDERED this work, when there was one
	// (ADR-0084 D2). It is a reference, never an identity: a Conversation is a long-lived
	// ordered exchange, a Session is one goal-scoped unit of work, and one conversation
	// spawns many sessions over its life. Empty for work that did not come from chat.
	//
	// The relationship was specified in ADR-0084 and then existed only in prose — neither
	// entity referenced the other — so nothing could answer "what did this conversation
	// actually set in motion?".
	ConversationID string `json:"conversation_id,omitempty"`
	// OriginMessageID is the specific turn that ordered the work. ConversationID gives
	// correlation ("part of the same exchange"); this gives causation ("caused by THIS
	// turn"), which is the distinction that makes the link useful for audit.
	OriginMessageID string `json:"origin_message_id,omitempty"`
}
