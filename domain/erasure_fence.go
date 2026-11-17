package domain

import (
	"context"
	"errors"
	"time"
)

// ADR-0137 S3–S5 (ENG-411 stage B): what the erasure authorization decides on, the
// no-resurrection fence's origin time, and the async tail's outbox port.

// ErrSourceErased is returned by a store write that would re-create content erased
// after the work that produced the write began (ADR-0137 D5): the write's ingest origin
// predates a tombstone of its document (or of its content digest), or the parent chunk
// a derivative hangs from no longer exists. Callers treat it as final — drop, count,
// never requeue: retrying cannot make the erased source exist again.
var ErrSourceErased = errors.New("source erased")

// ingestOriginKey carries the ingest origin time.
type ingestOriginKey struct{}

// WithIngestOrigin returns ctx carrying the instant the act that causes an ingest was
// accepted (an RPC's receipt, a signal's received time, an ingress delivery's
// ingested_at). It is a context VALUE, so it survives context.WithoutCancel and rides
// every async continuation of the ingest (AGENTS.md §2.7). A zero t is ignored.
func WithIngestOrigin(ctx context.Context, t time.Time) context.Context {
	if t.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, ingestOriginKey{}, t.UTC())
}

// IngestOriginFrom returns the ingest origin carried by ctx.
func IngestOriginFrom(ctx context.Context) (time.Time, bool) {
	t, ok := ctx.Value(ingestOriginKey{}).(time.Time)
	return t, ok && !t.IsZero()
}

// EnsureIngestOrigin returns ctx with an ingest origin: the one it already carries (a
// door that knows an earlier origin — a replayed signal, a retried delivery — stamped
// it), else now. The kernel's ingest doors call it at acceptance.
func EnsureIngestOrigin(ctx context.Context) context.Context {
	if _, ok := IngestOriginFrom(ctx); ok {
		return ctx
	}
	return WithIngestOrigin(ctx, time.Now())
}

// ErasureCandidate is one stored record an erasure selector resolves to, with what the
// authorization decision needs and nothing more: ids, the writer of record and the
// classification tags (ADR-0137 D3). Never text, a digest or a value — a candidate list
// is the content-free basis of a preview (job C) as well as of the decision.
type ErasureCandidate struct {
	// Kind is the record kind (ErasureTarget*: document, chunk, experience, entity).
	Kind string
	// ID is the record id. For a thread selector the candidates are its documents.
	ID string
	// Via is the selector entry that named this record (the thread id for a document
	// resolved through a thread; else ID).
	Via string
	// DocumentID is the document the record belongs to ("" when none).
	DocumentID string
	// WrittenBy is the kernel-stamped writer of record (documents.written_by, the chunk's
	// written_by metadata). "" when the record has none: rows written before the field
	// existed, kernel-authored experiences, substrate entities — erasable only by an
	// operator (ADR-0137 D3).
	WrittenBy string
	// Tags is the record's stored classification (owner tags included), the input of
	// the read-scope check.
	Tags []string
}

// AuthzRef implements Taggable.
func (c ErasureCandidate) AuthzRef() ResourceRef {
	return ResourceRef{Kind: KindMemory, ID: c.Kind + ":" + c.ID}
}

// AuthzTags implements Taggable.
func (c ErasureCandidate) AuthzTags() []string { return c.Tags }

var _ Taggable = ErasureCandidate{}

// ErasureOutboxRow is one pending async step of an executed erasure (D7).
type ErasureOutboxRow struct {
	ID        int64
	RequestID string
	Step      string
	Ref       string
	Attempts  int
}

// ErasureOutbox is the async tail's store (memory_erasure_outbox), drained by the one
// erasure worker. Every transition is exactly-once (ADR-0105 D3 shape).
type ErasureOutbox interface {
	// PendingSteps returns up to limit unprocessed rows with fewer than maxAttempts
	// attempts, oldest first.
	PendingSteps(ctx context.Context, limit, maxAttempts int) ([]ErasureOutboxRow, error)
	// CIDStillReferenced reports whether a surviving row still references a CAS blob
	// (evidence content hash, a source-document body), so it must not be deleted: the
	// store is content-addressed and identical bytes are shared.
	CIDStillReferenced(ctx context.Context, cid string) (bool, error)
	// VaultBodyStillReferenced reports whether a surviving run-ledger entry still
	// names an artifact-vault body (ADR-0157 D5.6): the vault is content-addressed.
	VaultBodyStillReferenced(ctx context.Context, hash string) (bool, error)
	// MarkStepDone marks a row processed (false when another drainer already did).
	MarkStepDone(ctx context.Context, id int64) (bool, error)
	// MarkStepFailed records one failed attempt with an error CLASS (never content).
	MarkStepFailed(ctx context.Context, id int64, errClass string) error
	// CompleteRequests marks every pending_async request whose steps are all processed
	// complete, returning how many changed.
	CompleteRequests(ctx context.Context) (int, error)
	// PruneTombstones deletes tombstone rows erased before cutoff, returning the count.
	PruneTombstones(ctx context.Context, cutoff time.Time) (int, error)
}

// VaultEraser deletes one artifact-vault body by its hash (satisfied by
// *vault.ArtifactVault). Deleting an absent hash is not an error.
type VaultEraser interface {
	Delete(hash string) error
}

// ContentEraser deletes one content-addressed blob by CID (the ADR-0137 D7 targeted
// delete; ContentStore.GC is not one). Deleting an absent CID is not an error.
type ContentEraser interface {
	DeleteContent(ctx context.Context, cid CID) error
}
