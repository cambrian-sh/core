package memory

import (
	"context"
	"encoding/json"

	"github.com/cambrian-sh/core/domain"
)

// Neighbor-window expansion (ADR-0060). A retrieved chunk is small; the passage
// that actually answers the query may sit in the chunk just before/after it, or
// straddle the boundary. So for each returned chunk we also pull its document
// neighbors (preceding/following, from the chunk_relations metadata the ingest
// path already writes) and include them. Cheap — id lookups, no model, no LLM.

// EnableNeighborWindow turns on neighbor-window expansion.
func (q *QueryService) EnableNeighborWindow() { q.neighborWindow = true }

// neighborIDs pulls a chunk's preceding/following neighbor ids from its
// chunk_relations metadata (jsonb — round-trips as a nested map, or a raw JSON
// string on some write paths; handle both).
func neighborIDs(meta map[string]interface{}) []string {
	raw, ok := meta["chunk_relations"]
	if !ok {
		return nil
	}
	var cr struct {
		PrecedingChunkID string `json:"preceding_chunk_id"`
		FollowingChunkID string `json:"following_chunk_id"`
	}
	switch v := raw.(type) {
	case string:
		_ = json.Unmarshal([]byte(v), &cr)
	case []byte:
		_ = json.Unmarshal(v, &cr)
	case map[string]interface{}:
		b, _ := json.Marshal(v)
		_ = json.Unmarshal(b, &cr)
	}
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
	for _, r := range results {
		for _, nid := range neighborIDs(r.Document.Metadata) {
			if nid == "" || seen[nid] {
				continue
			}
			doc, err := q.vectorStore.GetByID(ctx, nid)
			if err != nil || doc == nil {
				continue
			}
			seen[nid] = true
			out = append(out, domain.SearchResult{Document: *doc, Score: r.Score * 0.999})
		}
	}
	return out
}
