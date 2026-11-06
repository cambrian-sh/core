package domain

import (
	"context"
	"errors"
	"strconv"
	"time"
)

// Per-agent skill access grants (owner decision 2026-11-05,
// cambrian-knowledge/decisions/skill-access-grants.md; amends ADR-0139 D13):
//
//	"when an agent tries to reach a skill from the kernel, the operator should
//	 approve, and after that, that agent should always have access to that skill."
//
// A profile agent's EFFECTIVE skill set is its profile's `skills` list plus its
// active grants. A grant is keyed by agent id — not by the profile's source hash
// or version — so it survives profile edits, and it lasts until an operator
// revokes it. Grants come from three places (SkillGrant.Source):
//
//   - request:  the agent's use_skill on a skill outside its effective set opened a
//     skill_access approval on the operator approval hub and it was approved;
//   - proposal: an operator approved the agent's own skill proposal (D13);
//   - operator: an operator granted it directly.
//
// Ports and entities only; the rules live in internal/skill/access and the
// durable adapter in internal/infrastructure/postgres (PgSkillGrantStore).

// Skill grant sources.
const (
	SkillGrantSourceRequest  = "request"
	SkillGrantSourceProposal = "proposal"
	SkillGrantSourceOperator = "operator"
)

// Skill access values. pending/granted/denied are also the request states
// (SkillAccessRequest.State); all five are what the agent plane reports in
// cambrian.proto SkillDescriptor.access. "" there means the kernel does not bound
// this agent's skills (a non-profile agent).
const (
	SkillAccessProfile     = "profile"     // in the agent's profile skills
	SkillAccessGranted     = "granted"     // an active grant
	SkillAccessRequestable = "requestable" // visible, outside the effective set; use_skill asks an operator
	SkillAccessPending     = "pending"     // asked; no decision in time — stays visible to operators
	SkillAccessDenied      = "denied"      // asked; an operator denied it (or no grant store exists)
)

// Skill grant audit actions (skill_grant_events.action).
const (
	SkillGrantActionGranted   = "granted"
	SkillGrantActionRevoked   = "revoked"
	SkillGrantActionRequested = "requested"
	SkillGrantActionDenied    = "denied"
)

// ApprovalKindSkillAccess is the ApprovalRequest.Kind of a skill access request on
// the shared operator approval hub ("" = a dangerous-tool approval, ADR-0039).
const ApprovalKindSkillAccess = "skill_access"

// ErrSkillGrantNotFound is returned when revoking a grant that is not active.
var ErrSkillGrantNotFound = errors.New("skill grant: no active grant")

// SkillGrant is one agent's durable access to one skill. One row per
// (agent, skill); a revoke stamps RevokedAt, a later grant clears it.
type SkillGrant struct {
	ID           string // SkillGrantID(agent, skill) — deterministic (AGENTS.md §2.6)
	AgentID      string
	SkillName    string
	Source       string
	GrantedBy    string
	GrantedAt    time.Time
	Reason       string
	RevokedAt    time.Time // zero = active
	RevokedBy    string
	RevokeReason string
}

// Active reports whether the grant is in force.
func (g SkillGrant) Active() bool { return g.RevokedAt.IsZero() }

// SkillAccessRequest is the latest access request of one agent for one skill.
// One row per (agent, skill): a re-request reopens it.
type SkillAccessRequest struct {
	ID          string // SkillAccessRequestID(agent, skill)
	AgentID     string
	SkillName   string
	RunID       string
	Rationale   string
	State       string
	RequestedAt time.Time
	DecidedAt   time.Time
	DecidedBy   string
}

// SkillGrantEvent is one append-only audit row of a grant or request change.
type SkillGrantEvent struct {
	ID        string // SkillGrantEventID(agent, skill, seq)
	AgentID   string
	SkillName string
	Seq       int64
	Action    string
	Source    string
	Actor     string
	Reason    string
	RunID     string
	At        time.Time
	// CommandID is the operator command that produced the event ("" otherwise);
	// unique when set, so an operator retry appends nothing.
	CommandID string
}

// SkillGrantID derives a grant's id from its natural key.
func SkillGrantID(agentID, skill string) string {
	return "sg-" + shortHash(agentID+"\x00"+skill)
}

// SkillAccessRequestID derives a request's id from its natural key.
func SkillAccessRequestID(agentID, skill string) string {
	return "sar-" + shortHash(agentID+"\x00"+skill)
}

// SkillGrantEventID derives an audit event's id from (agent, skill, seq): two
// writers racing for one sequence collide on the key instead of duplicating.
func SkillGrantEventID(agentID, skill string, seq int64) string {
	return "sge-" + shortHash(agentID+"\x00"+skill+"\x00"+strconv.FormatInt(seq, 10))
}

// SkillGrantFilter selects grants or requests. Empty fields do not filter.
type SkillGrantFilter struct {
	AgentID        string
	SkillName      string
	IncludeRevoked bool   // grants only
	State          string // requests only ("" = all)
}

// SkillGrantStore is the durable port. Every write appends exactly one audit
// event in the same transaction when it changes something, and none when it is a
// no-op (granting an active grant, revoking a revoked one).
type SkillGrantStore interface {
	// Grant makes g active (insert, or re-activate a revoked row), resolves the
	// pair's pending request to granted, and audits. changed=false = already active.
	Grant(ctx context.Context, g SkillGrant, commandID string) (changed bool, err error)
	// Revoke stamps the active grant revoked and audits. ErrSkillGrantNotFound
	// when there is no active grant.
	Revoke(ctx context.Context, agentID, skill, by, reason, commandID string, at time.Time) error
	// ActiveGrants lists one agent's active grants.
	ActiveGrants(ctx context.Context, agentID string) ([]SkillGrant, error)
	ListGrants(ctx context.Context, f SkillGrantFilter) ([]SkillGrant, error)

	// OpenRequest upserts the pair's request as pending (audited "requested").
	OpenRequest(ctx context.Context, r SkillAccessRequest) error
	// DenyRequest marks the pair's request denied (audited "denied").
	DenyRequest(ctx context.Context, agentID, skill, by string, at time.Time) error
	ListRequests(ctx context.Context, f SkillGrantFilter) ([]SkillAccessRequest, error)

	// EventByCommand finds the event an operator command produced (idempotency).
	EventByCommand(ctx context.Context, commandID string) (SkillGrantEvent, bool, error)
	ListEvents(ctx context.Context, f SkillGrantFilter, limit int) ([]SkillGrantEvent, error)
}
