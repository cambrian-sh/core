package domain

import (
	"context"
	"time"
)

// ENG-2122: the durable journal of post-ingest derivations.
//
// Fact extraction (ADR-0136 D4) and KG chunk-triplet extraction (ADR-0053) run from
// in-memory queues after an ingest is acknowledged. Before the journal a kernel
// restart, a failed LLM call or a full queue lost the job for good: the chunks were
// stored, the facts and triplets never appeared, and nothing re-queued them. The ingest
// now records one DerivationJob per kind before it saves the chunks; the job is
// completed (deleted) when its derivation finished or was refused for good by the
// erasure / source-version fence, rescheduled with backoff after a failure, and re-fed
// by the kernel's recovery loop after a restart.

// DerivationKind names one post-ingest derivation.
type DerivationKind string

const (
	// DerivationFacts is ADR-0136 D4 fact extraction (memory.FactExtractor).
	DerivationFacts DerivationKind = "facts"
	// DerivationKGTriplets is ADR-0053 per-chunk triplet extraction
	// (memory.ChunkTripletsBatcher).
	DerivationKGTriplets DerivationKind = "kg_triplets"
	// DerivationEmbed is the background embedding of an accepted large document
	// (ENG-2143): its staged chunks are embedded and saved slice by slice after the
	// ingest returned. Its completion records the document's fact and KG jobs.
	DerivationEmbed DerivationKind = "embed"
)

// DerivationJob is one journaled derivation of one stored document. It holds
// identifiers and the context the in-memory job carried — never chunk text, which a
// replay reads back through the enforcing read store.
type DerivationJob struct {
	Kind       DerivationKind
	DocumentID string
	// ContentDigest is the document version the job was queued for (ENG-501). A replay
	// is fenced with it (WithSourceVersion), and completion removes only the job of
	// this version, never a newer ingest's.
	ContentDigest string
	// ChunkIDs are the document's stored chunks in ingest order that the job still
	// has to derive. A KG job is fed in slices and its row shrinks to the chunks not
	// yet extracted after each finished slice (AdvanceDerivation, ENG-2111), so a
	// replay resumes where the previous run stopped; a fact job keeps every chunk (its
	// windows and content hash depend on the whole document).
	ChunkIDs []string
	// Writer is the principal the ingest wrote as; EvidenceID the ADR-0105 evidence
	// the facts link to (part of each fact's deterministic id, so a replay writes the
	// same ids); IngestOrigin the erasure fence's origin (ADR-0137 D5).
	Writer       PrincipalRef
	EvidenceID   EvidenceID
	IngestOrigin time.Time
	// Source is the A2.1 source metadata the fact prompt renders.
	Source SourceMeta
	// Attempts is the failed extraction attempts so far (set by the journal on read).
	Attempts int
	// Staged is the opaque state an embed job resumes from (ENG-2143): the accepted
	// document's prepared chunks and structure rows, encoded by internal/memory. Set on
	// DerivationEmbed jobs only. Unlike the other kinds it holds document content, so
	// erasure deletes it with the document's rows. ChunksTotal is the chunk count of the
	// accepted version: progress is ChunksTotal - len(ChunkIDs) of ChunksTotal.
	Staged      []byte
	ChunksTotal int
}

// DerivationKey names one journaled job version.
type DerivationKey struct {
	Kind          DerivationKind
	DocumentID    string
	ContentDigest string
}

// Key returns the job's key.
func (j DerivationJob) Key() DerivationKey {
	return DerivationKey{Kind: j.Kind, DocumentID: j.DocumentID, ContentDigest: j.ContentDigest}
}

// DerivationBacklog is the journal's pending work of one kind: Pending rows not yet
// finished (queued in memory or due for recovery), Retrying the subset waiting out a
// failure's backoff, Dead the rows past their last allowed attempt, and Oldest the
// creation time of the oldest live row (zero when none).
type DerivationBacklog struct {
	Pending, Retrying, Dead int
	Oldest                  time.Time
}

// DerivationJournal is the durable store of DerivationJobs. postgres.PgVectorAdapter
// implements it over the derivation_jobs table. It is a kernel-internal bookkeeping
// table: rows carry identifiers and are never shown to a principal.
type DerivationJournal interface {
	// RecordDerivations upserts the jobs, claimed by the running process until
	// now+lease. A job of an existing (kind, document) replaces it — a re-ingest is a
	// new act: attempts and dead state are reset.
	RecordDerivations(ctx context.Context, jobs []DerivationJob, lease time.Duration) error
	// CompleteDerivation deletes the job of exactly this version. A missing row is not
	// an error.
	CompleteDerivation(ctx context.Context, k DerivationKey) error
	// RescheduleDerivation makes the job of exactly this version due again at retryAt
	// with lastError. countAttempt adds a failed attempt; when the attempts then reach
	// maxAttempts (> 0) the job is marked dead instead. It reports whether it died.
	RescheduleDerivation(ctx context.Context, k DerivationKey, retryAt time.Time, lastError string, countAttempt bool, maxAttempts int) (dead bool, err error)
	// AdvanceDerivation records the progress of the job of exactly this version: its
	// chunk ids become remaining (the chunks not yet derived), so a replay resumes
	// there (ENG-2111). Nothing else changes; a missing row is not an error.
	AdvanceDerivation(ctx context.Context, k DerivationKey, remaining []string) error
	// ClaimDerivations takes up to limit live jobs of kind that are orphaned (claimed
	// before since — the running process's start) or due (next_attempt_at <= now),
	// oldest due first, and claims them until now+lease. Concurrent claimers never
	// take the same row.
	ClaimDerivations(ctx context.Context, kind DerivationKind, since time.Time, limit int, lease time.Duration) ([]DerivationJob, error)
	// DerivationBacklog counts the journal's rows per kind.
	DerivationBacklog(ctx context.Context) (map[DerivationKind]DerivationBacklog, error)
	// RenewDerivation records the progress of the job of exactly this version (its
	// chunk ids become remaining) and extends its claim until now+lease (ENG-2143: a
	// background embed holds its row for as long as it works). It reports whether that
	// row still exists: false means the document was erased or a newer version replaced
	// the job, and the caller stops.
	RenewDerivation(ctx context.Context, k DerivationKey, remaining []string, lease time.Duration) (live bool, err error)
	// DeadDerivations lists up to limit jobs past their last attempt (dead), most
	// recently dead first, for the operator (ENG-2139). Identifiers and the error
	// only: never the job's staged content.
	DeadDerivations(ctx context.Context, limit int) ([]DeadDerivation, error)
	// RequeueDerivation makes the DEAD job of exactly this version live again: no
	// attempts, due now, so the recovery loop claims it on its next pass. It
	// reports false when no dead job of that version exists (already requeued,
	// completed, erased or replaced), so a repeated requeue is a no-op.
	RequeueDerivation(ctx context.Context, k DerivationKey) (bool, error)
	// IngestStatus reads the ingest state of one stored document (ENG-2143): the
	// background embed job of the document's current version when one exists, else
	// searchable. found is false when no document carries the id.
	IngestStatus(ctx context.Context, documentID string) (st IngestStatus, found bool, err error)
}

// DeadDerivation is one job past its last attempt, as the operator sees it
// (ENG-2139): its key, how often it failed, the last error and when it died.
type DeadDerivation struct {
	Key        DerivationKey
	Attempts   int
	LastError  string
	ChunkCount int
	CreatedAt  time.Time
	DeadAt     time.Time
}

// IngestState is where an ingested document stands (ENG-2143).
type IngestState string

const (
	// IngestAccepted: the document is stored (evidence, document row) and its chunks
	// are staged for background embedding; not yet searchable.
	IngestAccepted IngestState = "accepted"
	// IngestEmbedding: background embedding has stored part of the chunks.
	IngestEmbedding IngestState = "embedding"
	// IngestSearchable: every chunk is embedded and stored.
	IngestSearchable IngestState = "searchable"
	// IngestFailed: background embedding failed its last allowed attempt; Error says why.
	IngestFailed IngestState = "failed"
)

// IngestStatus is the visible progress of one document's ingest (ENG-2143).
type IngestStatus struct {
	DocumentID string
	State      IngestState
	// ChunksTotal and ChunksEmbedded count the accepted version's chunks while it is
	// embedded in the background (both 0 for a document whose chunks were stored with
	// the call, and once the background work finished).
	ChunksTotal, ChunksEmbedded int
	// Error is the last failure's cause (set for failed, and for a job retrying).
	Error string
	// Writer is the document's writer of record (documents.written_by), for an
	// agent-plane read only the writer may make.
	Writer string
}

// Terminal reports whether the state no longer changes on its own.
func (s IngestStatus) Terminal() bool {
	return s.State == IngestSearchable || s.State == IngestFailed
}
