package memory

import (
	"context"
	"errors"
	"time"
)

// ErrDocumentWriteDenied is returned when the decision point refuses to classify a
// document write. Deliberately coarse — the specific reason travels in the
// AccessDecision, which the chokepoint logs, so the error cannot be used to probe
// the vocabulary.
var ErrDocumentWriteDenied = errors.New("authz: document write denied by policy")

// SourceDocument is the ingested document itself — the thing a chunk came FROM
// (ADR-0093).
//
// Before this existed, a document had no representation in the database at all:
// parentage lived in a chunk-id string convention (`{docID}-chunk-{n}`) and a
// metadata key, and classification tags were copied onto every chunk at ingest
// with no authoritative row to copy from. Re-tagging meant updating N rows with no
// transaction boundary, so a partial failure left a document half-classified —
// some chunks reachable, some not.
//
// Tags live here now. A boundary that can be half-applied is not a boundary.
type SourceDocument struct {
	ID         string
	Title      string
	SourceType string
	// Text is the full document body. Retained so a policy decision, an audit, or a
	// re-chunk can be made against the document rather than reassembled from pieces.
	Text string
	// Tags is the AUTHORITATIVE classification. The copies carried in chunk metadata
	// are a derived cache and must never be written independently of this.
	Tags     []string
	Metadata map[string]any
}

// DocumentStore persists the source-document entity.
//
// It is a port of its own rather than a method on the vector store because a
// document is not a retrieval unit — it has no embedding and is never returned by
// a search. Folding it into the vector store would have put it back in the recall
// path this split exists to clean up.
type DocumentStore interface {
	// SaveDocument persists the document and returns the classification that was
	// actually stored.
	//
	// It returns the tags rather than swallowing them because the document row is
	// AUTHORITATIVE (ADR-0093 D4): the per-chunk copies are a derived cache, and a
	// cache derived from anything other than its source is just a second opinion.
	// The caller stamps chunks with exactly what landed here.
	SaveDocument(ctx context.Context, doc SourceDocument) ([]string, error)
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
// Separate from DocumentStore because listing is a read with completely different
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
func (im *IngestionManager) SetDocumentStore(store DocumentStore) {
	if im != nil && store != nil {
		im.documentStore = store
	}
}
