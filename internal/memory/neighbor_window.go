package memory

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/cambrian-sh/core/domain"
)

// Neighbor-window expansion (ADR-0060). A retrieved chunk is small; the passage
// that actually answers the query may sit in the chunk just before/after it, or
// straddle the boundary. So for each returned chunk we also pull its document
// neighbors (preceding/following, from the chunk_relations metadata the ingest
// path already writes) and include them. Cheap — id lookups, no model, no LLM.

// EnableNeighborWindow turns on neighbor-window expansion.
func (q *QueryService) EnableNeighborWindow() { q.neighborWindow = true }

// chunkRelationsOf parses a chunk's chunk_relations metadata (jsonb — round-trips
// as a nested map, or a raw JSON string on some write paths; handle both). A missing
// or malformed payload reads as the zero value (no neighbours).
func chunkRelationsOf(meta map[string]interface{}) ChunkRelations {
	var cr ChunkRelations
	raw, ok := meta["chunk_relations"]
	if !ok {
		return cr
	}
	switch v := raw.(type) {
	case string:
		_ = json.Unmarshal([]byte(v), &cr)
	case []byte:
		_ = json.Unmarshal(v, &cr)
	case json.RawMessage:
		_ = json.Unmarshal(v, &cr)
	case map[string]interface{}:
		b, _ := json.Marshal(v)
		_ = json.Unmarshal(b, &cr)
	}
	return cr
}

// neighborIDs pulls a chunk's preceding/following neighbor ids from its
// chunk_relations metadata.
func neighborIDs(meta map[string]interface{}) []string {
	cr := chunkRelationsOf(meta)
	out := make([]string, 0, 2)
	if cr.PrecedingChunkID != "" {
		out = append(out, cr.PrecedingChunkID)
	}
	if cr.FollowingChunkID != "" {
		out = append(out, cr.FollowingChunkID)
	}
	return out
}

// applyNeighborWindow expands each result with its document neighbors, deduped.
// A neighbor inherits just below its anchor's score so it sits next to it. Runs
// last (on the already-selected, ranked results), so it adds context without
// perturbing ranking.
func (q *QueryService) applyNeighborWindow(ctx context.Context, results []domain.SearchResult) []domain.SearchResult {
	if !q.neighborWindow || q.vectorStore == nil || len(results) == 0 {
		return results
	}
	seen := make(map[string]bool, len(results)*2)
	for _, r := range results {
		seen[r.Document.ID] = true
	}
	// Anchors FIRST, in their ranked order — so a downstream top-k truncation keeps
	// the real ranking intact. Neighbors are appended AFTER all anchors (bonus
	// context that only survives when the caller's k exceeds the anchor count);
	// they must never displace a ranked answer out of the returned window.
	out := make([]domain.SearchResult, 0, len(results)*2)
	out = append(out, results...)
	// ENG-83: every neighbour id in ONE GetBatch on the enforcing store (scope on
	// ctx, ADR-0095 D9), then appended in the same anchor-then-neighbour order the
	// per-id reads produced.
	type want struct {
		id    string
		score float64
	}
	var wants []want
	var ids []string
	for _, r := range results {
		for _, nid := range neighborIDs(r.Document.Metadata) {
			if nid == "" || seen[nid] {
				continue
			}
			seen[nid] = true
			wants = append(wants, want{nid, r.Score * 0.999})
			ids = append(ids, nid)
		}
	}
	if len(ids) == 0 {
		return out
	}
	docs, err := q.vectorStore.GetBatch(ctx, ids)
	if err != nil {
		slog.WarnContext(ctx, "neighbor window: batch read failed; no neighbours added", "err", err, "ids", len(ids))
		return out
	}
	byID := make(map[string]domain.Document, len(docs))
	for _, d := range docs {
		byID[d.ID] = d
	}
	for _, w := range wants {
		if d, ok := byID[w.id]; ok {
			out = append(out, domain.SearchResult{Document: d, Score: w.score})
		}
	}
	return out
}
