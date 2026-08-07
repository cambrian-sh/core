package domain

import "context"

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
