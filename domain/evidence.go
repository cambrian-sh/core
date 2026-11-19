package domain

import (
	"context"
	"errors"
	"fmt"
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
	// Parties are the entity identities this record is ABOUT — the customer, the
	// assignee, the supplier (ADR-0121 D2). Classification says what a record IS;
	// this says who it concerns, which is what lets a policy express "…and only
	// the rows you are a party to".
	//
	// A fact about the record, never a policy (INV-4). Empty means nobody is a
	// party, so a party-scoped policy admits it to no one — which is the safe
	// direction and what every row predating this field carries.
	Parties []string
	// Cursor and TraceID are delivery-technical fields. The technical envelope
	// TERMINATES at evidence (memo §19.2): derived records link here rather
	// than copying any of this.
	Cursor  string
	TraceID string
	// RevisesID links a newer revision of the same source artifact to the row
	// it supersedes. Empty for a first revision.
	RevisesID EvidenceID
	// DerivedFrom is the evidence row whose processing fetched this one (a
	// pipeline fetch node, ADR-0114 D10). A derived row carries exactly its
	// parent's Classification and Parties (ENG-1611). Empty for a delivery.
	DerivedFrom EvidenceID
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
	// Parties are the entity identities this delivery is ABOUT (ADR-0121 D2),
	// derived at the ingress from the record's own fields the same way
	// classification is — the source already states who its parties are.
	Parties []string
	Cursor  string
	TraceID string
	// RevisesID links this delivery to the evidence row it supersedes, when the
	// source itself declared a revision relationship.
	RevisesID EvidenceID
	// DerivedFrom names the evidence row that caused this material to be fetched
	// (ENG-1611). The ingestor then gives the new row its parent's
	// Classification and Parties, never anything wider: a caller may leave both
	// empty (the fetch knows no labels of its own) or repeat the parent's, and a
	// delivery naming other labels, a missing parent or a parent in another
	// namespace is refused (fail closed: a fetched page of a classified source
	// must never land readable by anyone).
	DerivedFrom EvidenceID
}

// ErrDerivedEvidence refuses a derived delivery whose labels cannot be the
// parent's (ENG-1611).
var ErrDerivedEvidence = errors.New("derived evidence")

// EvidenceOutboxItem is one unit of pending post-ingest work, inserted in the
// SAME transaction as its evidence row (transactional outbox, memo §11) and
// consumed at-least-once by the transformation stage (Phase 2).
type EvidenceOutboxItem struct {
	ID          int64
	NamespaceID string
	EvidenceID  EvidenceID
	CreatedAt   time.Time
	// Attempts is how many times processing this item has FAILED (not counting
	// not-ready holds, which are not failures). The consumer gives an item
	// EvidenceOutboxMaxAttempts tries, with jittered exponential backoff between
	// them, then parks it dead (ADR-0105 D3 amendment, owner 2026-11-12).
	Attempts int
}

// DeadEvidenceOutboxItem is an outbox item that exhausted its attempts: parked,
// never retried automatically, and listed so a person can see it (Ingress
// Studio's ListDeliveryFailures). The evidence and its bytes are untouched — the
// archive still holds what arrived (ADR-0105 D2).
type DeadEvidenceOutboxItem struct {
	EvidenceOutboxItem
	SourceID  string
	LastError string
	DeadAt    time.Time
}

// The evidence outbox's retry policy (ADR-0105 D3 / ADR-0108 D3 amendment, owner
// decision 2026-11-12): the pipeline runtime's own numbers
// (the premium pipeline runtime's DefaultRetryPolicy) so an item is retried
// the same way on either ingest path — 5 attempts, full-jitter exponential backoff
// from 500 ms capped at 5 min, then a terminal dead state with the last error.
// Mirrored, not shared: premium's policy is unreachable from core. Change one,
// change both.
const (
	EvidenceOutboxMaxAttempts = 5
	EvidenceOutboxBaseDelay   = 500 * time.Millisecond
	EvidenceOutboxMaxDelay    = 5 * time.Minute
	// EvidenceOutboxHoldRecheck is how long a NOT-READY item stays out of the
	// pending read when nothing releases it. Its release is the normal way back
	// (ReleaseHeldOutbox, e.g. on an ingress resume); this is the safety net for
	// a release that raced the hold or never came (a paused ingress that was
	// retired or re-mapped instead of resumed). Re-checking costs no attempt.
	EvidenceOutboxHoldRecheck = 30 * time.Minute
)

// ErrEvidenceNotReady is the sentinel a transformer's error wraps when an item
// cannot be processed NOW by design — e.g. its ingress is paused ("paused means
// paused", ADR-0112) — as opposed to failing. The consumer HOLDS such an item:
// it leaves the pending read, consumes no attempt, and comes back when
// ReleaseHeldOutbox names its hold key. Return one with EvidenceNotReady.
var ErrEvidenceNotReady = errors.New("evidence not ready for transformation")

// NotReadyError carries the hold key a not-ready item is parked under: the
// handle the transformer's owner later releases (an ingress's evidence source id).
type NotReadyError struct {
	HoldKey string
	Reason  string
}

func (e *NotReadyError) Error() string {
	return fmt.Sprintf("not ready (hold %q): %s", e.HoldKey, e.Reason)
}

// Is makes errors.Is(err, ErrEvidenceNotReady) true for any NotReadyError.
func (e *NotReadyError) Is(target error) bool { return target == ErrEvidenceNotReady }

// EvidenceNotReady builds the typed not-ready signal. holdKey must be non-empty:
// an item held under no key could only ever come back by the safety recheck.
func EvidenceNotReady(holdKey, reason string) error {
	return &NotReadyError{HoldKey: holdKey, Reason: reason}
}

// EvidenceErasureFence is the read half of the ADR-0137 D5 no-resurrection fence
// on the evidence lane, offered by an EvidenceStore that enforces it
// (postgres.PgEvidenceStore). SourceErased reports whether a tombstone of
// sourceKey was written after the ingest origin ctx carries (domain.IngestOriginFrom;
// false when ctx carries none), so the ingestor can refuse a replayed delivery
// BEFORE it writes the erased bytes back into the content store. It is only an
// early answer: the authoritative check runs again inside Insert's transaction,
// under the per-document lock the erase takes exclusively.
type EvidenceErasureFence interface {
	SourceErased(ctx context.Context, sourceKey string) (bool, error)
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

	// LatestBySourceKey returns the most recently ingested evidence row for one
	// source artifact — (namespace, source_id, source_key) — or nil when the
	// source never sent that key. It is how a writer finds the row a new
	// revision revises (ADR-0105 D4, ADR-0147 D7): source_revision is a digest
	// with no order, so "latest" is ingest order.
	LatestBySourceKey(ctx context.Context, namespace, sourceID, sourceKey string) (*Evidence, error)

	// PendingOutbox lists the outbox items that are DUE, oldest first, up to
	// limit: not processed, not dead, and past their next-attempt time (a
	// failed item waits out its backoff; a held, not-ready item waits for its
	// release or its recheck time). Listing does not claim: consumption is only
	// the MarkProcessed transition.
	PendingOutbox(ctx context.Context, limit int) ([]EvidenceOutboxItem, error)

	// RecordOutboxFailure records one FAILED attempt: attempts becomes
	// `attempts`, last_error is kept, and the item is either due again after
	// retryAfter or, when dead, parked terminally. A no-op on an item already
	// processed (another consumer finished it).
	RecordOutboxFailure(ctx context.Context, outboxID int64, attempts int, lastErr string, retryAfter time.Duration, dead bool) error

	// HoldOutbox parks a NOT-READY item under holdKey until ReleaseHeldOutbox
	// names that key or recheckAfter passes. Attempts are not touched: not
	// ready is not a failure.
	HoldOutbox(ctx context.Context, outboxID int64, holdKey, reason string, recheckAfter time.Duration) error

	// ReleaseHeldOutbox makes every unprocessed item held under holdKey due
	// now, and returns how many it released.
	ReleaseHeldOutbox(ctx context.Context, holdKey string) (int, error)

	// DeadOutbox lists dead (exhausted, unprocessed) items whose evidence came
	// from sourceID ("" = every source), most recently dead first, up to limit.
	DeadOutbox(ctx context.Context, sourceID string, limit int) ([]DeadEvidenceOutboxItem, error)

	// ReplayDeadOutbox makes the dead items whose evidence came from sourceID
	// pending again — attempts 0, last error and hold cleared, due now — so the
	// ordinary consumer re-runs them (owner decision 2026-11-13,
	// T-20261113-dead-evidence-outbox-items-have-no-replay-path: no second
	// processing path). evidenceID narrows it to that one delivery's item
	// ("" = every dead item of the source). sourceID is required: a replay is
	// always scoped to one source, and an evidence id from another source
	// matches nothing. Returns the items it replayed as they were when dead
	// (attempts, last error, dead time); an item that is not dead (pending,
	// held, processed, or already replayed) is left alone, so replaying twice
	// is a no-op the second time.
	ReplayDeadOutbox(ctx context.Context, sourceID string, evidenceID EvidenceID) ([]DeadEvidenceOutboxItem, error)

	// MarkProcessed performs the exactly-once-logical outbox transition.
	// It returns true when THIS call performed the transition and false when
	// the item was already processed — the caller that sees false must treat
	// the work as someone else's and produce no effect.
	MarkProcessed(ctx context.Context, outboxID int64) (bool, error)
}
