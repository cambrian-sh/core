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

// SourceDocumentMarker is the literal tag that introduces a document's IDENTITY in
// a caller's tag list: the term immediately following it is the document id
// (`externalDocumentID` rule 1). Exported so the identity convention has ONE
// definition that both the ingest path and the write chokepoints read.
const SourceDocumentMarker = "source_document"

// ClassificationHint strips the IDENTITY terms out of a caller's tag list, leaving
// only the terms that are candidate CLASSIFICATIONS (ADR-0099).
//
// Why this must run before the write chokepoint asks the Authorizer to classify:
// `ExternalDocument.Tags` carries two different things at once. A classification is
// what the document IS, drawn from the controlled vocabulary, and the only thing
// access policy can act on. An identity NAMES one document — six benchmark suites
// and the operator upload lane pass `source_document` + a unique id deliberately,
// because the id-less fallback once collapsed N documents onto one chunk id and
// silently destroyed data.
//
// Handing an identity term to classification derivation is a category error with a
// loud failure: a controlled vocabulary rejects it as coinage and the whole write is
// DENIED, so a document is unwritable for carrying its own name. That is what this
// prevents. It is the same shape as the provenance exemption on the premium side —
// terms that were never a classification attempt are removed rather than judged.
//
// Unlike the ingest-path filter this does not take the resolved document id, so it
// consumes the successor of the marker unconditionally. That IS rule 1: the term
// after `source_document` is the id by construction. A trailing marker with no
// successor consumes nothing.
func ClassificationHint(tags []string) []string {
	if len(tags) == 0 {
		return tags
	}
	out := make([]string, 0, len(tags))
	for i := 0; i < len(tags); i++ {
		if tags[i] == SourceDocumentMarker {
			if i+1 < len(tags) {
				i++ // the id it introduced is identity, never a classification
			}
			continue
		}
		out = append(out, tags[i])
	}
	return out
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
