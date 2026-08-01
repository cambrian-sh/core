package domain

import (
	"context"
	"time"
)

// Evidence is one immutable record of WHAT ARRIVED — the first stage of the
// knowledge substrate's epistemic pipeline (ADR-0105; memo §6):
//
//	Evidence → KnowledgeItem → Assessment → Resolution → Effect
//
// It is written BEFORE any semantic processing, referencing original bytes that
// are already durable in the ContentStore, so a failed extraction never loses
// the source material and any later extractor can re-run from the archive.
//
// Immutability is structural: the store port has no update method. A changed
// source artifact is NEW evidence with a higher SourceRevision linked by
// RevisesID (ADR-0105 D4) — otherwise "what did we receive, and when" stops
// being answerable, which is the whole point of the layer.
type Evidence struct {
	ID EvidenceID
	// NamespaceID is constant-valued today ('default') and carried from day one
	// because retrofitting it across every substrate table and unique index
	// later is the expensive path (ADR-0105 D5).
	NamespaceID string
	// SourceID identifies the producing source ("slack:workspace-x",
	// "erp:acme"). SourceKey is the artifact's source-NATIVE identity (message
	// id, row key, file path); SourceRevision its revision there. The triple is
	// the idempotency key inside a namespace: replaying the same delivery
	// creates no new version (ADR-0105 D3).
	SourceID       string
	SourceKey      string
	SourceRevision string
	// Two clocks, deliberately separate (memo §8). SourceTime is what the
	// sender's clock claimed — attacker-controllable, never used for latency.
	// IngestedAt is when Cambrian received it: the "could we have known?"
	// clock, and the compliance-defence answer.
	SourceTime time.Time
	IngestedAt time.Time
	// ContentHash points into the ContentStore; the bytes are durable before
	// this record exists (content-first commit ordering, ADR-0105 D2).
	ContentHash  CID
	ContentBytes int64
	// Classification as delivered by the source. Derived records inherit
	// monotonically from it (memo §12); they never widen it.
	Classification []string
	// Cursor and TraceID are delivery-technical fields. The technical envelope
	// TERMINATES at evidence (memo §19.2): derived records link here rather
	// than copying any of this.
	Cursor  string
	TraceID string
	// RevisesID links a newer revision of the same source artifact to the row
	// it supersedes. Empty for a first revision.
	RevisesID EvidenceID
}

// EvidenceID identifies one evidence row.
type EvidenceID string

// RawEvidence is one delivery to be preserved as evidence. Transport-shaped on
// purpose: no interpretation, no derived fields — interpretation belongs to the
// pipeline stages that read the archive, never to the archiving step.
type RawEvidence struct {
	NamespaceID    string
	SourceID       string
	SourceKey      string
	SourceRevision string
	SourceTime     time.Time
	Bytes          []byte
	Classification []string
	Cursor         string
	TraceID        string
	// RevisesID links this delivery to the evidence row it supersedes, when the
	// source itself declared a revision relationship.
	RevisesID EvidenceID
}

// EvidenceOutboxItem is one unit of pending post-ingest work, inserted in the
// SAME transaction as its evidence row (transactional outbox, memo §11) and
// consumed at-least-once by the transformation stage (Phase 2).
type EvidenceOutboxItem struct {
	ID          int64
	NamespaceID string
	EvidenceID  EvidenceID
	CreatedAt   time.Time
}

// EvidenceStore persists evidence rows and their outbox items.
//
// The port's guarantees are decomposed per layer and deliberately NOT summed
// into "exactly once" (ADR-0105 D3): insertion is idempotent by source
// revision key; the outbox transition is exactly-once logically (a second
// consumer of the same item observes false); delivery to a consumer is
// at-least-once.
type EvidenceStore interface {
	// Insert atomically records ev and one outbox item for it. The caller MUST
	// hold a verified ContentHash before calling — the store trusts the
	// ordering contract rather than re-reading the blob.
	//
	// Idempotent: if (namespace, source_id, source_key, source_revision)
	// already exists, Insert writes nothing and returns the EXISTING row's id
	// with inserted=false. A replayed delivery therefore never mints a
	// duplicate evidence version and never enqueues duplicate work.
	Insert(ctx context.Context, ev Evidence) (id EvidenceID, inserted bool, err error)

	// Get returns the evidence row, or an error when absent.
	Get(ctx context.Context, id EvidenceID) (*Evidence, error)

	// PendingOutbox lists unconsumed outbox items, oldest first, up to limit.
	// Listing does not claim: consumption is only the MarkProcessed transition.
	PendingOutbox(ctx context.Context, limit int) ([]EvidenceOutboxItem, error)

	// MarkProcessed performs the exactly-once-logical outbox transition.
	// It returns true when THIS call performed the transition and false when
	// the item was already processed — the caller that sees false must treat
	// the work as someone else's and produce no effect.
	MarkProcessed(ctx context.Context, outboxID int64) (bool, error)
}
