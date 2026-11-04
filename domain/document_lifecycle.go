package domain

import (
	"context"
	"errors"
	"time"
)

// ErrDocumentOwnedByAnotherWriter refuses an ingest whose document id is already held
// by a DIFFERENT writer of record (ADR-0138 D1.2, Q1). Before ADR-0138 the second writer
// silently took the rows over: its chunk upsert replaced metadata.source_agent_id, and
// the first principal lost read access to its own content. Callers match it with
// errors.Is; the gRPC doors map it to PermissionDenied.
var ErrDocumentOwnedByAnotherWriter = errors.New("document id is owned by another writer")

// DocumentRecord is what the lifecycle knows about one stored document: its writer of
// record and the digest of the content it holds. Content-free.
type DocumentRecord struct {
	ID string
	// Exists is false when no documents row carries ID.
	Exists bool
	// WrittenBy is documents.written_by, else the source_agent_id its chunks carry (rows
	// written before ADR-0138), else "" (a writer-less, kernel-internal ingest).
	WrittenBy string
	// ContentDigest is the ADR-0138 D1.3 revision digest of the stored version; "" for a
	// row written before ADR-0138.
	ContentDigest string
	// Version is the 1-based version counter (1 for a row written before ADR-0138).
	Version int
	// Title is the stored document title (an operator upload's file name).
	Title string
	// CreatedAt is when the document was first written; UpdatedAt when its stored
	// version was (the latest ingest). Zero when unknown.
	CreatedAt time.Time
	UpdatedAt time.Time
}

// DocumentRevision is one ingest's claim on a document id (ADR-0138 D3).
type DocumentRevision struct {
	ID string
	// Writer is the kernel-stamped writer (ExternalDocument.Author); "" for a writer-less
	// ingest, which neither claims nor is refused.
	Writer string
	// ContentDigest is the revision digest of the incoming content (ADR-0138 D1.3).
	ContentDigest string
	// Text is the incoming Body. Compared only against a legacy row that has no digest.
	Text string
	// HasData is true for a binary document (bytes in ExternalDocument.Data): a legacy row
	// without a digest cannot prove it holds the same bytes, so it counts as changed.
	HasData bool
}

// DocumentCascadeCounts is what one document cascade deleted, per store (content-free;
// ADR-0138 D4). Keys are table names.
type DocumentCascadeCounts map[string]int

// DocumentRevisionOutcome reports what ReviseDocument did.
type DocumentRevisionOutcome struct {
	// Previous is the stored record before this ingest (Exists=false for a new document).
	Previous DocumentRecord
	// Retired is true when the stored version's content and every derivative were deleted
	// because the incoming content differs.
	Retired bool
	// Version is the version the incoming content becomes (1 for a new document).
	Version int
	// Counts is what the retirement deleted; nil when nothing was retired.
	Counts DocumentCascadeCounts
}

// DocumentLifecycleStore owns a document's identity checks and its version chain
// (ADR-0138 D2–D4).
//
// It is a port of its own, not a method on DocumentStore, because its one write — the
// retirement — deletes across every derived lane (chunks, KG triplets, PageRank,
// structure, fact items, assessments, markers) in ONE transaction, which only a store
// holding both the vector tables and the knowledge substrate can do.
type DocumentLifecycleStore interface {
	// DocumentRecord returns what is stored under id (Exists=false when nothing is).
	DocumentRecord(ctx context.Context, id string) (DocumentRecord, error)
	// ReviseDocument is called before an ingest writes id. In one transaction it refuses a
	// different writer of record (ErrDocumentOwnedByAnotherWriter), does nothing for a new
	// document or unchanged content, and otherwise retires the stored version — its
	// chunks and everything derived from them — records it as a content-free version row,
	// and advances the version. The documents row itself is kept.
	ReviseDocument(ctx context.Context, rev DocumentRevision) (DocumentRevisionOutcome, error)
	// FindByContent returns the oldest stored document of writer whose current version
	// has exactly this revision digest and still holds content (ADR-0138 B1.1: an
	// exact re-upload is a no-op). Exists=false when there is none, or when writer or
	// digest is empty. Rows written before 0026 carry no digest and are never found.
	FindByContent(ctx context.Context, writer, digest string) (DocumentRecord, error)
	// RecordAssociation records that a document was uploaded again (or first) in a
	// session and classification scope (ADR-0138 B1.1). Idempotent per (document,
	// writer, session, scope); it never changes the document, its chunks or its tags.
	RecordAssociation(ctx context.Context, a DocumentAssociation) error
}

// DocumentAssociation is one (document, session, scope) an upload was delivered in
// (ADR-0138 B1.1). Content-free bookkeeping: an exact duplicate writes no chunks, so
// this row is what says the operator uploaded it there too. It does NOT widen or
// narrow who may read the document — classification stays the stored tags.
type DocumentAssociation struct {
	DocumentID string
	Writer     string
	// SessionID is the upload's session ("" for none).
	SessionID string
	// Scope is the upload's classification scope (normalised, sorted).
	Scope []string
	// UploadName is the file name as uploaded (not normalised).
	UploadName string
}

// IngestOutcome is what one ingest did, for a caller that must report more than the
// returned id (the operator plane's IngestMemoryOpResponse, ADR-0138 B1). The caller
// puts an empty one on ctx (WithIngestOutcome); the ingestion manager fills it. Absent
// from ctx, nothing is recorded.
type IngestOutcome struct {
	// DocumentID is the document the ingest resolved to (documents.id).
	DocumentID string
	// DuplicateOf is set when the ingest was an exact duplicate of a stored document of
	// the same writer (B1.1): nothing was written, and this is that document's id.
	DuplicateOf string
	// Version is the document's version after the ingest (1 for a new document; >1
	// when this ingest replaced an earlier version, ADR-0138 D3).
	Version int
	// Replaced is true when this ingest retired a stored version.
	Replaced bool
}

type ingestOutcomeKey struct{}

// WithIngestOutcome returns ctx carrying out, which the ingest fills.
func WithIngestOutcome(ctx context.Context, out *IngestOutcome) context.Context {
	return context.WithValue(ctx, ingestOutcomeKey{}, out)
}

// IngestOutcomeFrom returns the outcome on ctx, or nil.
func IngestOutcomeFrom(ctx context.Context) *IngestOutcome {
	out, _ := ctx.Value(ingestOutcomeKey{}).(*IngestOutcome)
	return out
}
