package domain

import (
	"context"
	"time"
)

// AuditFilter constrains an audit query (ADR-0047 0047-08). Zero fields match all.
type AuditFilter struct {
	Actor      string
	TargetType string
	TargetID   string
	ActionType string
	Limit      int
}

// AuditStore persists operator-mutating actions (ADR-0047 D15). Record is the
// idempotency gate: a repeated CommandID is recorded once. The port lives in
// domain so both the operator adapter and the Postgres infra adapter depend on
// it (clean hexagonal direction), not on each other.
type AuditStore interface {
	// Record persists entry. If an entry with the same CommandID already exists,
	// it returns deduped=true and does not insert (idempotency).
	Record(ctx context.Context, entry AuditEntry) (deduped bool, err error)
	// Query returns matching entries, most recent first.
	Query(ctx context.Context, f AuditFilter) ([]AuditEntry, error)
}

// AuditEntry is the immutable record of one operator-mutating action (ADR-0047
// D15). CommandID is the client-generated idempotency key (unique); the audit
// write IS the dedup. Before/After are the command handler's transactional view
// of the mutated entity, serialized.
type AuditEntry struct {
	ID         string
	CommandID  string
	At         time.Time
	Actor      string
	Role       string
	ActionType string
	TargetType string
	TargetID   string
	Before     string
	After      string
	Reason     string
	Result     string
}
