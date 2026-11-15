package domain

import (
	"context"
	"time"
)

// Memory-store ports and the plain types they carry, declared in domain so the
// Postgres adapter implements them without importing the internal/memory service
// (owner decision 2026-11-14, ENG-1104; AGENTS.md §1.3: adapters depend on domain
// ports, never on services). internal/memory keeps type aliases for its callers.

// SectionRow is a structural section node persisted as a documents row.
type SectionRow struct {
	ID              string // "<documentID>::sec:<path>"
	DocumentID      string // the source document id
	Title           string
	Level           int
	Order           int
	SectionPath     string // human breadcrumb
	SectionLtree    string // ltree ordinal path (e.g. "n0.n2")
	ParentSectionID string // parent section row id, or "" if under the document root
}

// ChunkStamp inherits a leaf's structural location onto an ingested chunk.
type ChunkStamp struct {
	ChunkID         string
	SectionPath     string
	SectionLtree    string
	ParentSectionID string
}

// StructuralEdge is a typed structure-graph edge (PART_OF / NEXT).
type StructuralEdge struct {
	SourceID string
	TargetID string
	Type     EdgeType
}

// StructureGraphStore persists the assembled structure graph. Implemented by the
// pgvector adapter; nil = structure graph disabled (legacy).
type StructureGraphStore interface {
	SaveSections(ctx context.Context, sections []SectionRow) error
	StampChunks(ctx context.Context, stamps []ChunkStamp) error
	SaveStructuralEdges(ctx context.Context, edges []StructuralEdge) error
}

// DocumentFilter narrows a document listing.
//
// Deliberately not a query string. Enumeration answers a question SEARCH cannot:
// "which of my documents have no labels?" has no query text, because the operator
// does not yet know what those documents say — that is precisely why they are
// unlabelled.
type DocumentFilter struct {
	// Limit is the page size. The store clamps it; 0 means the default page.
	Limit int
	// Cursor is keyset pagination: the last id of the previous page, exclusive.
	// Keyset rather than OFFSET so a concurrent ingest cannot shift the window and
	// silently skip a row — the one row an operator most needs to see is a NEW
	// document, which is exactly what an offset page drops.
	Cursor string
	// UnlabelledOnly restricts the listing to documents no rule can reach.
	UnlabelledOnly bool
	// IDPrefix is a cheap prefix filter. Not full-text: that is QueryMemory's job.
	IDPrefix string
	// Tags restricts the listing to documents carrying ALL of these labels
	// (contract 0075). It is what lets an interview option say "61 documents"
	// against the same source the documents screen reads, so the two can never
	// disagree about the size of a scope the operator is choosing between.
	//
	// Intersection rather than union on purpose: a scope is a narrowing, and OR
	// would make adding a second label WIDEN it, which is the opposite of what
	// selecting two labels means to the person doing it.
	Tags []string
	// Owners are the principals whose owner-scoped documents (owner:<id>, the
	// owner fence, domain/owner_scope.go) this listing may include. Empty — the
	// operator plane, an unresolved caller — lists NO owner-scoped document:
	// titles and labels of a person's mailbox are that person's, not the
	// deployment's. There is deliberately no "all owners" switch.
	//
	// Ignored when Scope is set: the predicate carries its own owners.
	Owners []string
	// Scope is the reader's resolved read predicate (ADR-0085), pushed into the
	// listing's SQL through internal/authz/sqlfence — the full predicate
	// (required, forbidden, any-of, party, owner fence, own-rows), filtered
	// BEFORE the page limit and shared by the count, so neither the page nor
	// total_matching can disclose a row the reader may not read (ENG-725). nil
	// is the operator plane's listing: the owner fence over Owners only. A
	// principal-serving caller (the MCP list_documents tool) must always set it.
	Scope *TagPredicate
}

// DocumentSummary is one LISTING row — no body, no chunks.
//
// The document text is deliberately absent. A listing exists to decide what to
// label across a whole corpus, and carrying N document bodies to render a table of
// ids would make the cheapest question in the system one of the most expensive.
type DocumentSummary struct {
	ID         string
	Title      string
	SourceType string
	Tags       []string
	ChunkCount int
	CreatedAt  time.Time
}

// DocumentLister enumerates documents by ROW rather than by relevance.
//
// Separate from domain.DocumentStore because listing is a read with completely different
// cost characteristics from a write, and a store that could not list was the gap
// that made unlabelled documents unfindable: policy acts on labels, so a document
// with none is not denied — it is invisible to the policy model, and nothing in the
// console could enumerate the invisible set.
type DocumentLister interface {
	// ListDocuments returns one page, the cursor for the next page (empty when the
	// listing is exhausted), and the total number of documents matching the filter
	// ignoring paging.
	//
	// The total is returned alongside the page because "50 shown" does not tell an
	// operator how much of the corpus no rule can reach, and "422 of 1163" does.
	ListDocuments(ctx context.Context, filter DocumentFilter) (page []DocumentSummary, nextCursor string, totalMatching int, err error)
}

// ChunkEntities is one chunk and the entities it mentions (the deduped union of
// its triplet heads + tails, lowercased). The input unit for ComputePageRank.
type ChunkEntities struct {
	ChunkID  string
	Entities []string
}

// Ingest-timing stages the Postgres adapter reports inside chunk_save (on the
// domain.StageClock riding ctx). Declared here so the adapter needs no import of
// internal/memory, whose ingestion_timing.go re-exports them (ENG-1104).
const (
	StageChunkSaveBegin   = "chunk_save_begin"      // pool acquire + BEGIN inside chunk_save
	StageChunkSaveUpsert  = "chunk_save_upsert"     // chunk row upserts inside chunk_save
	StageChunkSaveProject = "chunk_save_projection" // ADR-0107 projection upserts inside chunk_save; 0 since ENG-335 (one pgx.Batch, reported as chunk_save_upsert)
	StageChunkSaveCommit  = "chunk_save_commit"     // COMMIT inside chunk_save
)
