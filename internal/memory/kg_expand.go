package memory

import (
	"context"
	"log/slog"
	"sort"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// ChunkTripletsStore is the storage interface for the per-chunk triplets that
// the LLM extracts at write time. The KG²RAG retrieval pattern (ADR-0053 D3)
// reads these at query time for one-hop chunk expansion.
//
// The implementation lives in the postgres adapter; this interface is what the
// retrieval path depends on so it can be faked in unit tests.
type ChunkTripletsStore interface {
	// Save persists a list of triplets for a single chunk. Idempotent on
	// (chunk_id, h, r, t) — repeated inserts are no-ops.
	SaveChunkTriplets(ctx context.Context, chunkID string, triplets []ChunkTriplet) error

	// ForChunk returns the triplets extracted from a chunk (h, r, t, weight).
	ForChunk(ctx context.Context, chunkID string) ([]ChunkTriplet, error)

	// ForChunks batches the above — returns a map chunkID -> []ChunkTriplet.
	// Used by the KG expansion post-processor to walk many seed chunks in
	// one query.
	ForChunks(ctx context.Context, chunkIDs []string) (map[string][]ChunkTriplet, error)

	// ChunksMentioningEntity returns the chunk IDs that have a triplet
	// referencing the given entity (as either head or tail). This is the
	// "entity → chunks" lookup that powers the KG expansion.
	// Matching is case-insensitive (entities are stored lowercase).
	ChunksMentioningEntity(ctx context.Context, entity string, limit int) ([]string, error)
}

// kgExpand is the KG²RAG one-hop chunk expansion. It walks the per-chunk
// triplets from the seed chunks, builds a set of referenced entities, and
// pulls in the chunks that share those entities. The result is the union
// of seed + expanded chunks, deduplicated by chunk ID.
//
// This is the "associative trigger" in T-Mem's vocabulary — a chunk that
// mentions an entity mentioned in a seed chunk is reachable from the seed
// via the KG. The expansion is bounded to one hop (T-Mem's "first hop" in
// T-Mem's "two trigger families" framing). Two-hop expansion is a Layer 1+
// enhancement (ADR-0053 D5).
//
// Returns the expanded chunk set, ordered by:
//  1. Seed chunks first (in input order)
//  2. Then expanded chunks by descending cosine score (from the vector search)
//  3. Ties broken by chunk ID for determinism
func kgExpand(
	ctx context.Context,
	seeds []domain.SearchResult,
	store ChunkTripletsStore,
	vectorSearch kgExpandVectorSearch,
	queryVec []float32,
	opts kgExpandOpts,
) []domain.SearchResult {
	if len(seeds) == 0 || store == nil {
		return seeds
	}
	if opts.Hops <= 0 {
		opts.Hops = 1
	}
	if opts.MaxExpanded <= 0 {
		opts.MaxExpanded = 20
	}
	if opts.MaxEntities <= 0 {
		opts.MaxEntities = 30
	}
	if opts.PerEntity <= 0 {
		opts.PerEntity = 5
	}

	// Build the seen set from seeds (no duplicates in the input).
	seen := make(map[string]bool, len(seeds))
	for _, s := range seeds {
		seen[s.Document.ID] = true
	}
	// Output starts with seeds in their original order.
	out := make([]domain.SearchResult, 0, len(seeds)+opts.MaxExpanded)
	out = append(out, seeds...)

	// Walk Hops rounds of expansion. Most calls have Hops=1; the structure
	// supports deeper walks for future layering.
	frontier := seeds
	for hop := 0; hop < opts.Hops; hop++ {
		// Collect triplet entities from the frontier chunks.
		frontierIDs := make([]string, 0, len(frontier))
		for _, s := range frontier {
			frontierIDs = append(frontierIDs, s.Document.ID)
		}
		byChunk, err := store.ForChunks(ctx, frontierIDs)
		if err != nil {
			slog.Warn("kgExpand: ForChunks failed; expansion truncated", "err", err, "hop", hop)
			break
		}
		entityCounts := make(map[string]int) // entity -> frontier-chunk mentions
		entitySet := make(map[string]struct{})
		for _, triplets := range byChunk {
			for _, t := range triplets {
				if h := strings.TrimSpace(t.H); h != "" {
					entityCounts[h]++
					entitySet[h] = struct{}{}
				}
				if tt := strings.TrimSpace(t.T); tt != "" {
					entityCounts[tt]++
					entitySet[tt] = struct{}{}
				}
			}
		}
		if len(entitySet) == 0 {
			break // no entities to expand from
		}

		// Rank entities by mention frequency; take top MaxEntities.
		ranked := rankEntitiesByCount(entityCounts, opts.MaxEntities)

		// For each top entity, find chunks that reference it.
		nextFrontier := []domain.SearchResult{}
		budget := len(seeds) + opts.MaxExpanded
		for _, ent := range ranked {
			if len(out)+len(nextFrontier) >= budget {
				break
			}
			relatedIDs, err := store.ChunksMentioningEntity(ctx, ent, opts.PerEntity)
			if err != nil {
				slog.Warn("kgExpand: ChunksMentioningEntity failed", "entity", ent, "err", err)
				continue
			}
			for _, id := range relatedIDs {
				if seen[id] {
					continue
				}
				seen[id] = true
				// Score the entity-routed chunk: a 0.5 floor so the chunk
				// SURVIVES the downstream rerank (KG expansion exists to surface
				// chunks vector search missed), lifted by the query→chunk cosine
				// when the chunk is also query-relevant. expandedScore is the seam
				// the ADR-0054 Stage-A multi-signal blend extends.
				doc := mustGetDoc(ctx, vectorSearch, id)
				nextFrontier = append(nextFrontier, domain.SearchResult{
					Document: doc,
					Score:    expandedScore(queryVec, doc),
				})
				if len(out)+len(nextFrontier) >= budget {
					break
				}
			}
		}
		// Move newly-found chunks into out, then make them the next frontier
		// for further hops (if Hops > 1).
		out = append(out, nextFrontier...)
		frontier = nextFrontier
	}
	return out
}

// expandedScore scores a KG-routed (entity-reached) chunk. It floors at 0.5 so
// the chunk SURVIVES the downstream rerank — KG expansion exists precisely to
// surface chunks that vector search ranked low — but lifts a chunk that is ALSO
// query-relevant above the floor by its query→chunk cosine. A nil queryVec or a
// chunk with no materialized embedding falls back to the bare floor (prior
// behavior). This is the integration seam the ADR-0054 Stage-A blend extends:
// cosine becomes one of several weighted signals (recency, confidence, pagerank,
// activation), all preserving the survival floor.
func expandedScore(queryVec []float32, doc domain.Document) float64 {
	const floor = 0.5
	if len(queryVec) == 0 || len(doc.Embedding.Vector) == 0 {
		return floor
	}
	if s := cosineSimilarity(queryVec, doc.Embedding.Vector); s > floor {
		return s
	}
	return floor
}

// kgExpandVectorSearch is the minimum surface we need to materialize a
// SearchResult for an expanded chunk. The default impl in the postgres
// adapter uses a vector lookup; tests can use a map-backed fake.
type kgExpandVectorSearch interface {
	GetByID(ctx context.Context, id string) (*domain.Document, error)
}

// mustGetDoc returns the doc for an ID, or a minimal placeholder if the
// fetch fails. The expansion shouldn't fail the query just because a
// related-chunk fetch errored; the user still has the seed chunks.
func mustGetDoc(ctx context.Context, vs kgExpandVectorSearch, id string) domain.Document {
	doc, err := vs.GetByID(ctx, id)
	if err != nil || doc == nil {
		return domain.Document{ID: id}
	}
	return *doc
}

// kgExpandOpts configures the expansion depth and limits.
type kgExpandOpts struct {
	Hops        int // default 1
	MaxExpanded int // default 20 (max new chunks added)
	MaxEntities int // default 30 (max entities to consider from frontier)
	PerEntity   int // default 5 (max chunks pulled per entity via ChunksMentioningEntity)
}

// rankEntitiesByCount sorts entities by mention frequency desc, returns the
// top N as a slice.
func rankEntitiesByCount(counts map[string]int, n int) []string {
	type entry struct {
		entity string
		count  int
	}
	all := make([]entry, 0, len(counts))
	for e, c := range counts {
		all = append(all, entry{e, c})
	}
	// Sort by count desc, tie-break by entity string asc (determinism).
	// O(n log n) — the prior hand-rolled selection sort was O(n²) and blew up
	// when a large expansion frontier produced thousands of candidate entities.
	sort.Slice(all, func(i, j int) bool {
		if all[i].count != all[j].count {
			return all[i].count > all[j].count
		}
		return all[i].entity < all[j].entity
	})
	if n > 0 && n < len(all) {
		all = all[:n]
	}
	out := make([]string, len(all))
	for i, e := range all {
		out[i] = e.entity
	}
	return out
}
