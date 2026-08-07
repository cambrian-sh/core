package memory

import (
	"context"
	"log/slog"
	"sort"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

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
//
// AUTHORIZATION (ADR-0095 D9). Expansion reaches chunks BY ID, which does not pass
// through the one place reads are enforced: EnforcingVectorStore overrides Search and
// nothing else ("Search is the single SQL-building chokepoint"), so GetByID is an
// unguarded read of the raw adapter. ChunksMentioningEntity is likewise an unscoped
// lookup over chunk_triplets, a table with no classification column. Neither is changed
// here; instead every materialized chunk is checked against `scope` before admission, so
// the ID path honours the predicate the Search path honours.
//
// `scope` is a REQUIRED parameter rather than an opts field on purpose: a new call site
// must supply it, and a nil predicate denies everything (TagPredicate.Check fail-closes),
// so forgetting to wire it drops expansion rather than silently widening it.
func kgExpand(
	ctx context.Context,
	seeds []domain.SearchResult,
	store domain.ChunkTripletsStore,
	vectorSearch kgExpandVectorSearch,
	queryVec []float32,
	scope *domain.TagPredicate,
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
			relatedIDs, err := store.ChunksMentioningEntity(ctx, ent, opts.PerEntity, scope)
			if err != nil {
				slog.Warn("kgExpand: ChunksMentioningEntity failed", "entity", ent, "err", err)
				continue
			}
			for _, id := range relatedIDs {
				if seen[id] {
					continue
				}
				seen[id] = true
				// Materialize under the caller's read predicate. A chunk that is
				// missing, unreadable, or NOT PERMITTED is dropped outright — never
				// admitted as a stub. It is marked seen either way so a denial is not
				// retried on a later entity (and costs no extra query).
				doc, ok := authorizedDoc(ctx, vectorSearch, scope, id)
				if !ok {
					continue
				}
				// Score the entity-routed chunk: a 0.5 floor so the chunk
				// SURVIVES the downstream rerank (KG expansion exists to surface
				// chunks vector search missed), lifted by the query→chunk cosine
				// when the chunk is also query-relevant. expandedScore is the seam
				// the ADR-0054 Stage-A multi-signal blend extends.
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
// authorizedDoc materializes an expanded chunk and admits it only if the caller's
// read predicate allows its classification tags. ok=false means DROP — the chunk is
// missing, unreadable, or forbidden.
//
// It replaces mustGetDoc, whose `return domain.Document{ID: id}` fallback leaked. That
// fallback PREDATES authorization: it was correct when a missing row was the only way
// GetByID could fail, and ADR-0085 added a second reason — denial — which it then
// treated identically. The stub carried the restricted chunk's ID into the result pool
// (and `{docID}-chunk-{n}` ids encode their source document), while expandedScore floored
// it at 0.5 because a stub has no embedding. Worse, GetByID is not enforced at all, so a
// permitted-looking read returned the row's FULL CONTENT. Hence the check here rather
// than a nil-check: there is no denial signal to detect, so the predicate is applied
// directly to what came back.
//
// A nil predicate denies everything (TagPredicate.Check fail-closes on nil), matching
// readFilter's contract that a nil predicate means no read is authorized.
func authorizedDoc(ctx context.Context, vs kgExpandVectorSearch, scope *domain.TagPredicate, id string) (domain.Document, bool) {
	doc, err := vs.GetByID(ctx, id)
	if err != nil || doc == nil {
		return domain.Document{}, false
	}
	if !scope.Allows(docTags(doc.Metadata)) {
		return domain.Document{}, false
	}
	return *doc, true
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
