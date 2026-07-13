package memory

import (
	"sort"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ReRankWithAssociative re-ranks a candidate set using the T-Mem two-trigger
// formula:
//
//	score = cosine × (α + (1-α) × effective) + β × associative_reachability
//
// where:
//   - cosine is the candidate's current Score (post-BFS energy)
//   - α = 0.2 (the recall floor multiplier; ADR-0015)
//   - effective = TemporalDecay(activation, lastAccessed, lambda, now) — a
//     soft demotion of ancient memories so a doc with high cosine but a
//     quiet lastAccessed still ranks below a fresher cousin
//   - β defaults to 0.2 (associative weight; tunable via SetAssociativeWeight)
//   - associative_reachability = sum over the doc's entities of
//     (query_entity_cosine × doc_entity_weight), precomputed by the caller
//
// The reachability map is computed by the caller (the QueryService) so this
// function stays a pure re-rank. Input is not mutated; a sorted copy is
// returned. Preserves every candidate — nothing is dropped.
func ReRankWithAssociative(
	candidates []domain.SearchResult,
	reachability map[string]float64,
	lambda float64,
	beta float64,
	now time.Time,
) []domain.SearchResult {
	const floorAlpha = 0.2

	if beta < 0 {
		beta = 0
	}

	out := make([]domain.SearchResult, len(candidates))
	copy(out, candidates)

	for i := range out {
		cosine := out[i].Score
		effective := domain.TemporalDecay(
			out[i].Document.ActivationStrength,
			out[i].Document.LastAccessedAt,
			lambda,
			now,
		)
		reach := reachability[out[i].Document.ID]
		out[i].Score = cosine*(floorAlpha+(1-floorAlpha)*effective) + beta*reach
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// ComputeReachability walks every entity in the in-memory index, scores the
// query→entity cosine, and aggregates the result per doc. The per-doc value
// is the sum of (query_entity_cosine × doc_entity_weight) across the doc's
// entities, which the re-rank formula multiplies by β.
//
// Pure: takes a precomputed queryVec; the index is locked briefly per step.
// If entityIdx is nil the map is empty (no associative signal — fallback to
// surface similarity only). The cost is O(entities × docs_per_entity) under
// a read lock; for the LoCoMo benchmark's ~100 entities × ~10 docs/entity
// this is ~1k comparisons — negligible.
func ComputeReachability(
	candidates []domain.SearchResult,
	entityIdx *EntityIndex,
	queryVec []float32,
) map[string]float64 {
	out := make(map[string]float64, len(candidates))
	if entityIdx == nil || len(queryVec) == 0 {
		return out
	}

	// Step 1: score every entity by query→name cosine. O(entities) under
	// the read lock; copy the embeddings out before walking the index the
	// other way to avoid holding the lock across the inner loop.
	embs := entityIdx.SnapshotEmbeddings()
	entityScore := make(map[string]float64, len(embs))
	for key, emb := range embs {
		if len(emb.Vector) == 0 {
			continue
		}
		s := cosineSimilarity(queryVec, emb.Vector)
		if s > 0 {
			entityScore[key] = s
		}
	}
	if len(entityScore) == 0 {
		return out
	}

	// Step 2: for each candidate doc, find which entities mention it and
	// accumulate (query_entity_cosine × doc_entity_weight). DocsFor takes
	// the read lock per call; this is fine for the working set size.
	entityKeys := make([]string, 0, len(entityScore))
	for k := range entityScore {
		entityKeys = append(entityKeys, k)
	}
	for _, c := range candidates {
		id := c.Document.ID
		if id == "" {
			continue
		}
		var reach float64
		for _, key := range entityKeys {
			for _, assoc := range entityIdx.DocsFor(key) {
				if assoc.DocID == id {
					reach += entityScore[key] * assoc.Weight
					break
				}
			}
		}
		if reach > 0 {
			out[id] = reach
		}
	}
	return out
}
