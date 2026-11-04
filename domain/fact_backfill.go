package domain

import (
	"context"
	"time"
)

// ADR-0136 D4 backfill (T-20261103-no-backfill-path-populates-memory-facts-for; rebased
// onto the knowledge substrate by A1): the
// fact extractor runs only on new ingests, so a document stored before
// execution.facts.extract_enabled was on has no facts until something feeds it to the
// SAME extractor. The lister below is the one piece of that feed the extractor does not
// already have: which stored documents lack an extraction marker, in what order, and
// which chunk ids make each of them up.

// ProvenanceSourceTagPrefix is the classification tag the premium decision point stamps
// on every write a principal makes (authz.StampSourceProvenance in cambrian-premium):
// "provenance:source=<principal id>". The backfill filters by it; the kernel never
// writes it itself.
const ProvenanceSourceTagPrefix = "provenance:source="

// FactBackfillFilter selects the documents a backfill visits. Every set field narrows
// the selection (AND); the zero value selects every unmarked document.
type FactBackfillFilter struct {
	// SourceType matches documents.source_type exactly ("" = any).
	SourceType string
	// Principal matches a document written by that principal: its provenance tag
	// (ProvenanceSourceTagPrefix+Principal) or a chunk's source_agent_id ("" = any).
	Principal string
	// Namespace matches a principal-id PREFIX on the same two fields, e.g. the
	// LongMemEval per-run namespace "lme-<ns>-" ("" = any).
	Namespace string
	// Tags are classification tags the document must all carry ("" entries ignored).
	Tags []string
	// Limit caps the documents returned (0 = all).
	Limit int
	// IncludeMarked also returns documents that already have an extraction marker
	// for SOME content. By default those are excluded in SQL; with this on the caller
	// decides per document by content hash (the knowledge_extractions marker), which also
	// catches a document re-ingested with new content while extraction was off.
	IncludeMarked bool
	// ExtractorVersion is the running extractor's version (ADR-0136 A2.4). The SQL
	// exclusion above then drops only documents with a COMPLETE marker of this version;
	// a marker of another version, a legacy one or a partial one keeps the document a
	// candidate. "" = any marker excludes (the A1 behaviour).
	ExtractorVersion string
}

// FactBackfillCandidate is one stored document the backfill may extract. It carries
// identifiers and bookkeeping only — never content: the chunk TEXT is read through the
// enforcing read store (AGENTS.md §2.2), not through the lister.
type FactBackfillCandidate struct {
	DocumentID string
	// ChunkIDs are the document's stored content chunks in ingest order (the order
	// the extractor's content hash digests and its turn indexes count).
	ChunkIDs []string
	// SessionTime is what the ingest path hands the extractor: the document's stated
	// event time, else the time it was recorded.
	SessionTime time.Time
	// Tags is the document's classification (documents.tags). Only an ordering key
	// for the backfill: each fact takes the tags of its own SOURCE CHUNK, as at ingest.
	Tags       []string
	SourceType string
	// Author is the writing principal recorded on the chunks (source_agent_id, the
	// ingest path's doc.Author). It becomes the facts' actor — the resolution key's
	// actor (ADR-0136 A1-Q2) — so a backfilled fact and a later ingested one of the same
	// principal resolve together.
	Author string
	// ADR-0136 A2.1 source metadata, from the documents row (never content): its title,
	// and the time label of its session time (documents.time_source / time_precision),
	// from which the reference basis is derived (ReferenceBasisOf).
	Title         string
	TimeSource    TimeSource
	TimePrecision TimePrecision
	// TextChars is the total length of the content chunks (a cost estimate input:
	// A2.3 windows per document).
	TextChars int
	// SourceKind and AuthorLabel are the client's descriptive source kind and author
	// (ADR-0136 A3.2), from the documents row's metadata, so a backfill renders the
	// same Source block as the live ingest did. "" = the client gave none.
	SourceKind  string
	AuthorLabel string
}

// SourceMeta is the candidate's A2.1 source metadata, with the A3.2 precedence of
// SourceMetaOf: type = SourceKind else SourceType; author = AuthorLabel else the recorded
// writer (the ingest path's doc.Author, which is also its principal fallback).
func (c FactBackfillCandidate) SourceMeta() SourceMeta {
	return SourceMeta{
		SourceType: firstNonBlank(c.SourceKind, c.SourceType), Title: c.Title,
		Author:         firstNonBlank(c.AuthorLabel, c.Author),
		ReferenceTime:  c.SessionTime,
		ReferenceBasis: ReferenceBasisOf(c.TimeSource, c.SessionTime),
		TimePrecision:  c.TimePrecision,
	}
}

// FactResetFilter selects the memory_fact items a reset deletes (ADR-0136 A2-Q3,
// cmd/facts-reset). Namespace and Principal match the items' writing principal
// (knowledge_items.asserted_by) and, for the extraction markers, the documents' writer
// exactly as FactBackfillFilter does; at least one of them, or All, must be set.
type FactResetFilter struct {
	// Namespace is a principal-id PREFIX (e.g. a LongMemEval run namespace "lme-<ns>-").
	Namespace string
	// Principal is one exact principal id.
	Principal string
	// All selects every memory_fact item (an explicit opt-in: no selector is refused).
	All bool
	// KeepLearned keeps the learned memory_fact predicate profiles. By default they are
	// deleted (owner answer A2-Q2): profiles are namespace-wide, so votes learned from
	// the reset subset would otherwise survive it. Declared and override rows are kept.
	KeepLearned bool
	// DryRun counts what would be deleted and changes nothing.
	DryRun bool
}

// FactResetReport counts what a reset deleted (or, dry run, would delete).
type FactResetReport struct {
	Items       int // memory_fact items (their statement values and embeddings cascade)
	Resolutions int // resolution versions citing the items or on their keys
	Assessments int // assessments whose inputs overlap the items
	Markers     int // memory_fact extraction markers of the selected documents
	Profiles    int // learned predicate profile rows
	Rederived   int // surviving keys re-derived (cited by a deleted assessment, or of a deleted learned predicate)
}

// FactResetter is the hard reset of the fact layer for a re-run (A2-Q3). It is NOT
// ADR-0137 erasure: erasure leaves content-free sentinels so a closed chain stays
// closed; a reset removes the facts, their verdicts and their markers outright, so a
// later backfill extracts the documents again from scratch. postgres.PgKnowledgeStore
// implements it in one transaction.
type FactResetter interface {
	ResetFacts(ctx context.Context, f FactResetFilter) (FactResetReport, error)
}

// FactBackfillLister lists backfill candidates in SessionTime order (then document id).
// postgres.PgKnowledgeStore implements it over documents/chunks, excluding documents
// with a memory_fact marker in knowledge_extractions (ADR-0136 A1.7).
type FactBackfillLister interface {
	ListFactBackfillCandidates(ctx context.Context, f FactBackfillFilter) ([]FactBackfillCandidate, error)
}
