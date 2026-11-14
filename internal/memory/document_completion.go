package memory

import (
	"context"

	"github.com/cambrian-sh/core/domain"
)

// Document / session completion (owner decision 2026-11-14, third round;
// T-20261103-fact-extraction-yields-nothing-from-assistant, ENG-430).
//
// Some questions are about a whole piece of content that was retrieved only in
// part: "what was the 7th item in the list you gave me?" ranks one chunk of the
// assistant's list, and the item the question needs sits in a neighbouring chunk
// the ranking had no reason to pick. The owner's decision is that such content is
// covered by returning the rest of the document (an ingested chat session is one
// document), not by extracting facts from it.
//
// It generalises the ADR-0060 neighbour window (neighbor_window.go), which adds
// only the one chunk on each side, and uses the same chain: every chunk carries
// its preceding/following chunk id in chunk_relations metadata (ingestion_chunks.go).
// For each of the first `documents` distinct source documents in the ranked
// results, the chain is walked out from the best-ranked hit of that document in
// both directions, alternately, until `maxChunks` chunks are added or the document
// ends. A short document is therefore returned whole; a long one returns the
// window around the hit.
//
// Invariants, the neighbour window's:
//   - non-displacing: every completion chunk is appended AFTER all ranked rows,
//     so a downstream top-k truncation keeps the real ranking;
//   - enforced: chunks are read by id through q.vectorStore, which is the tier-1
//     EnforcingVectorStore in production (NewMemoryStack), with the caller's scope
//     already on ctx (searchByType) — a chunk the caller may not read is absent;
//   - cheap: id lookups only, no model, no LLM.
// Off by default; it is turned on only after an A/B/A (owner decision).

const (
	defaultCompletionDocuments = 1
	defaultCompletionMaxChunks = 24
)

type docCompletion struct {
	enabled   bool
	documents int
	maxChunks int
}

// EnableDocumentCompletion turns completion on (execution.retrieval.
// document_completion_*, wired in app.go). documents ≤ 0 ⇒ 1; maxChunks ≤ 0 ⇒ 24.
func (q *QueryService) EnableDocumentCompletion(documents, maxChunks int) {
	if documents <= 0 {
		documents = defaultCompletionDocuments
	}
	if maxChunks <= 0 {
		maxChunks = defaultCompletionMaxChunks
	}
	q.docCompletion = docCompletion{enabled: true, documents: documents, maxChunks: maxChunks}
}

// chainNeighbour returns the preceding (dir < 0) or following (dir > 0) chunk id of a
// chunk, "" at the end of its document.
func chainNeighbour(meta map[string]interface{}, dir int) string {
	cr := chunkRelationsOf(meta)
	if dir < 0 {
		return cr.PrecedingChunkID
	}
	return cr.FollowingChunkID
}

// applyDocumentCompletion appends the rest of the top documents after the ranked
// results, each document's added chunks in document order.
func (q *QueryService) applyDocumentCompletion(ctx context.Context, results []domain.SearchResult) []domain.SearchResult {
	c := q.docCompletion
	if !c.enabled || q.vectorStore == nil || len(results) == 0 {
		return results
	}
	seen := make(map[string]bool, len(results)+c.documents*c.maxChunks)
	for _, r := range results {
		seen[r.Document.ID] = true
	}
	out := append(make([]domain.SearchResult, 0, len(results)+c.documents*c.maxChunks), results...)
	completed := make(map[string]bool, c.documents)
	for _, anchor := range results {
		if len(completed) >= c.documents {
			break
		}
		docID := hitDocumentID(anchor.Document)
		if docID == "" || completed[docID] {
			continue // an episode or kernel artifact has no document to complete
		}
		completed[docID] = true

		var before, after []domain.SearchResult // before is collected nearest-first
		next := [2]string{chainNeighbour(anchor.Document.Metadata, -1), chainNeighbour(anchor.Document.Metadata, +1)}
		added := 0
		for added < c.maxChunks && (next[0] != "" || next[1] != "") {
			for side := 0; side < 2 && added < c.maxChunks; side++ {
				id := next[side]
				if id == "" {
					continue
				}
				next[side] = ""
				doc, err := q.vectorStore.GetByID(ctx, id)
				if err != nil || doc == nil {
					continue // hidden or gone: the chain stops on this side
				}
				if hitDocumentID(*doc) != docID {
					continue // a broken chain must not leak another document in
				}
				dir := -1
				if side == 1 {
					dir = +1
				}
				next[side] = chainNeighbour(doc.Metadata, dir)
				if seen[id] {
					continue // already a ranked row: keep walking past it
				}
				seen[id] = true
				added++
				row := domain.SearchResult{Document: *doc, Score: anchor.Score * 0.998}
				if side == 0 {
					before = append(before, row)
				} else {
					after = append(after, row)
				}
			}
		}
		for i := len(before) - 1; i >= 0; i-- {
			out = append(out, before[i])
		}
		out = append(out, after...)
	}
	return out
}
