package memory

import (
	"context"
	"errors"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ErrDocumentWriteDenied is returned when the decision point refuses to classify a
// document write. Deliberately coarse — the specific reason travels in the
// AccessDecision, which the chokepoint logs, so the error cannot be used to probe
// the vocabulary.
var ErrDocumentWriteDenied = errors.New("authz: document write denied by policy")

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

// SetDocumentStore wires the source-document entity onto the ingest path. Optional:
// a nil store means documents are not recorded and chunks keep a NULL parent, which
// is the pre-ADR-0093 behaviour rather than a failure.
func (im *IngestionManager) SetDocumentStore(store domain.DocumentStore) {
	if im != nil && store != nil {
		im.documentStore = store
	}
}

// ErrDocumentNotFound and DocumentGetter live in domain so the premium module can
// depend on them (internal/ is unimportable there). Aliased here because this
// package is where the other document ports live and a reader looking for them
// should find them, not a dead end.
var ErrDocumentNotFound = domain.ErrDocumentNotFound

// DocumentGetter is domain.DocumentGetter.
type DocumentGetter = domain.DocumentGetter
