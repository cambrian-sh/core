package domain

import (
	"context"
	"errors"
	"time"
)

// Read-only inspection of the fact layer for the operator plane (owner decision
// 2026-11-14 on T-20261103-operator-plane-cannot-inspect-facts-no-rpc: ListFacts,
// GetFactChain and GetFactCoverage, scope-checked, no override or backfill
// controls). The fact layer is ON by default (ENG-1496), so an operator must be able
// to see what it extracted, how a value's supersession chain was derived, and how
// much of the corpus has been run through the extractor.

// FactListFilter narrows ListFacts. Subject and Attribute are compared after
// NormalizeFactText against the normalised keys of the item's entity id
// (<scope_key>/<subject_key>/<predicate_key>); Actor is the writing principal
// (asserted_by). Limit <= 0 means the default page; After is the opaque cursor a
// previous page returned.
type FactListFilter struct {
	Subject   string
	Attribute string
	Actor     string
	Limit     int
	After     string
}

// ErrFactCursor reports a ListFacts page cursor the reader did not issue. It is
// refused, never ignored: ignoring it would silently restart at page one.
var ErrFactCursor = errors.New("fact list: invalid page cursor")

// Default and maximum page sizes for ListFacts.
const (
	FactListDefaultLimit = 50
	FactListMaxLimit     = 200
)

// FactExtractorVersionCoverage is how many documents carry an extraction marker of
// one extractor version: complete (every passage covered) or partial.
type FactExtractorVersionCoverage struct {
	Version           string // `<prompt id>/<generator model>`, "" for pre-0024 markers
	DocumentsComplete int64
	DocumentsPartial  int64
	LastExtractedAt   time.Time
}

// FactCoverage is the extraction coverage of the corpus: documents with stored
// chunks, documents carrying any memory_fact marker, stored memory_fact items, and
// the per-version breakdown. Counts only — no content.
type FactCoverage struct {
	DocumentsWithChunks int64
	DocumentsExtracted  int64
	FactItems           int64
	LastExtractedAt     time.Time
	Versions            []FactExtractorVersionCoverage
}

// FactInspector is the operator plane's read port over memory_fact items. Every read
// applies scope IN SQL exactly as FactItemReader does; the kernel reaches it only
// through authz.EnforcingFactInspector, which refuses a read with no predicate.
type FactInspector interface {
	// ListFacts returns stored items (stated bounds, no derivation — use FactChain for
	// the derived intervals) newest assertion first, and the cursor of the next page
	// ("" when this is the last).
	ListFacts(ctx context.Context, f FactListFilter, scope *TagPredicate) ([]Fact, string, error)
	// FactsByID returns the stored items with these ids under scope (FactStagedReader).
	FactsByID(ctx context.Context, ids []KnowledgeItemID, scope *TagPredicate) ([]Fact, error)
	// FactChain returns the derived chain of f's resolution key under scope
	// (FactItemReader.FactChain).
	FactChain(ctx context.Context, f Fact, scope *TagPredicate) ([]Fact, error)
	// FactCoverage reports extraction coverage (counts only).
	FactCoverage(ctx context.Context) (FactCoverage, error)
}
