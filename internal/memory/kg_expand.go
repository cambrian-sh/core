package memory

import (
	"context"
	"log/slog"
	"math"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/cambrian-sh/core/domain"
)

// foldDiacritics strips combining marks so "kraków" and "krakow" compare equal
// (NFD-decompose, drop marks). Mirrors the Python planner's _fold_marks.
func foldDiacritics(s string) string {
	decomposed := norm.NFD.String(s)
	out := make([]rune, 0, len(decomposed))
	for _, r := range decomposed {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		out = append(out, r)
	}
	return string(out)
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

		// Alias linking: widen the entity set with (a) deterministic lexical
		// variants (possessive/plural, hyphen↔space, diacritics) so "US Army's"
		// and "us army" meet, and (b) embedding-nearest entity names from the
		// EntityIndex when one is wired — the query-time half of HippoRAG-style
		// synonym linking, at zero extra embed calls (the index already holds
		// name embeddings). Bounded: variants are cheap strings; semantic
		// neighbors only for the top few entities.
		expanded := expandEntityAliases(ranked, opts.AliasIndex, opts.MaxEntities*2)

		// Candidate gathering. Preferred path: ONE batched, relevance-ranked,
		// corroboration-counting lookup per hop (ChunksForEntities capability) —
		// this replaced a loop of up to MaxEntities sequential per-entity queries
		// ordered by extraction recency (review defects Q8 + Q16). Fallback: the
		// legacy per-entity loop for stores without the capability.
		nextFrontier := []domain.SearchResult{}
		budget := len(seeds) + opts.MaxExpanded
		room := budget - len(out)
		if room <= 0 {
			break
		}
		type candidate struct {
			id      string
			matches int
		}
		var candidates []candidate
		if batched, ok := store.(entityBatchLookup); ok {
			// Over-ask modestly: some hits are already seen or will be denied.
			hits, err := batched.ChunksForEntities(ctx, expanded, room*2+len(seeds), scope, queryVec)
			if err != nil {
				slog.Warn("kgExpand: ChunksForEntities failed; expansion truncated", "err", err, "hop", hop)
				break
			}
			for _, h := range hits {
				if seen[h.ChunkID] {
					continue
				}
				candidates = append(candidates, candidate{id: h.ChunkID, matches: h.Matches})
			}
		} else {
			// Legacy path: per-entity queries, corroboration counted client-side.
			order := []string{}
			mentions := map[string]int{}
			for _, ent := range expanded {
				relatedIDs, err := store.ChunksMentioningEntity(ctx, ent, opts.PerEntity, scope)
				if err != nil {
					slog.Warn("kgExpand: ChunksMentioningEntity failed", "entity", ent, "err", err)
					continue
				}
				for _, id := range relatedIDs {
					if seen[id] {
						continue
					}
					if mentions[id] == 0 {
						order = append(order, id)
					}
					mentions[id]++
				}
			}
			// Corroborated chunks first; stable on first-seen (entity-rank) order.
			sort.SliceStable(order, func(a, b int) bool { return mentions[order[a]] > mentions[order[b]] })
			for _, id := range order {
				candidates = append(candidates, candidate{id: id, matches: mentions[id]})
			}
		}

		// Materialize under the caller's read predicate — BATCHED (review Q8):
		// one chunks-only round-trip when the store supports it, replacing a
		// per-candidate GetByID loop that probed four tables each. A chunk that
		// is missing, unreadable, or NOT PERMITTED is absent from the map and
		// dropped — never admitted as a stub; it is marked seen either way so a
		// denial is not retried on a later hop.
		candIDs := make([]string, len(candidates))
		for i, c := range candidates {
			candIDs[i] = c.id
		}
		docs := materializeAuthorized(ctx, vectorSearch, scope, candIDs)
		for _, cand := range candidates {
			if len(out)+len(nextFrontier) >= budget {
				break
			}
			seen[cand.id] = true
			doc, ok := docs[cand.id]
			if !ok {
				continue
			}
			nextFrontier = append(nextFrontier, domain.SearchResult{
				Document: doc,
				Score:    expandedScore(queryVec, doc, hop, cand.matches),
			})
		}
		// Move newly-found chunks into out, then make them the next frontier
		// for further hops (if Hops > 1).
		out = append(out, nextFrontier...)
		frontier = nextFrontier
	}
	return out
}

// entityBatchLookup is the optional store capability behind the batched,
// relevance-ranked, corroboration-counting expansion path (one SQL round-trip
// per hop). Implemented by the pgvector adapter; fakes fall back to the
// per-entity loop.
type entityBatchLookup interface {
	ChunksForEntities(ctx context.Context, entities []string, limit int, scope *domain.TagPredicate, queryVec []float32) ([]domain.EntityChunkHit, error)
}

// aliasNeighborIndex is the optional alias source: embedding-nearest entity
// names for a given name, from an index that already holds name embeddings.
type aliasNeighborIndex interface {
	NeighborNamesOf(name string, k int) []string
}

// expandEntityAliases widens an entity list with lexical variants and (when an
// index is wired) embedding-nearest entity names, deduplicated, capped at max.
// Order preserves the original ranking: originals first, then their variants in
// rank order — so a cap trims aliases before it ever trims a real entity.
func expandEntityAliases(ranked []string, idx aliasNeighborIndex, max int) []string {
	if max <= 0 {
		max = len(ranked)
	}
	out := make([]string, 0, max)
	seen := map[string]bool{}
	add := func(e string) {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" || seen[e] || len(out) >= max {
			return
		}
		seen[e] = true
		out = append(out, e)
	}
	for _, e := range ranked {
		add(e)
	}
	// Semantic neighbors for the top few entities only — the head of the
	// ranking carries the query's real subjects; hub noise lives in the tail.
	const aliasTopEntities = 5
	const aliasPerEntity = 2
	if idx != nil {
		for i, e := range ranked {
			if i >= aliasTopEntities || len(out) >= max {
				break
			}
			for _, n := range idx.NeighborNamesOf(e, aliasPerEntity) {
				add(n)
			}
		}
	}
	for _, e := range ranked {
		if len(out) >= max {
			break
		}
		for _, v := range lexicalAliasVariants(e) {
			add(v)
		}
	}
	return out
}

// lexicalAliasVariants generates cheap deterministic spelling variants of an
// entity name: possessive/plural stripped, hyphen↔space swapped, diacritics
// folded. These are the most common reasons two mentions of the same entity
// never meet in a spaCy-extracted vocabulary.
func lexicalAliasVariants(e string) []string {
	e = strings.ToLower(strings.TrimSpace(e))
	if e == "" {
		return nil
	}
	var out []string
	push := func(v string) {
		if v != "" && v != e {
			out = append(out, v)
		}
	}
	if strings.HasSuffix(e, "'s") {
		push(strings.TrimSuffix(e, "'s"))
	} else if strings.HasSuffix(e, "s") && len(e) > 3 {
		push(strings.TrimSuffix(e, "s"))
	}
	if strings.Contains(e, "-") {
		push(strings.ReplaceAll(e, "-", " "))
	} else if strings.Contains(e, " ") {
		push(strings.ReplaceAll(e, " ", "-"))
	}
	if folded := foldDiacritics(e); folded != e {
		push(folded)
	}
	return out
}

// expandedScore scores a KG-routed (entity-reached) chunk. The base floor keeps
// the chunk SURVIVING the downstream rerank — KG expansion exists precisely to
// surface chunks that vector search ranked low — but the flat 0.5 of the
// original design ranked a 2nd-hop, single-hub-entity chunk identically to a
// direct, multiply-corroborated one. Now the floor DECAYS with hop distance
// (structural confidence drops as the walk lengthens) and gains a bounded
// corroboration bonus (a chunk matched by several distinct frontier entities is
// better graph evidence than one reached via a single hub — the KG²RAG
// organization signal at scoring time). A chunk that is ALSO query-relevant is
// lifted above its floor by the query→chunk cosine, exactly as before. All
// deterministic; the non-displacing assembly still means these scores order
// injected chunks among THEMSELVES and can never evict a primary hit.
func expandedScore(queryVec []float32, doc domain.Document, hop, corroboration int) float64 {
	floor := 0.5 * math.Pow(0.8, float64(hop))
	if extra := corroboration - 1; extra > 0 {
		if extra > 4 {
			extra = 4
		}
		floor += 0.05 * float64(extra)
	}
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

// batchChunkFetcher is the optional batched-materialization capability
// (review Q8): one chunks-only round-trip for a whole candidate set.
// Implemented by the pgvector adapter and forwarded by the enforcing store.
type batchChunkFetcher interface {
	ChunksByIDs(ctx context.Context, ids []string) ([]domain.Document, error)
}

// materializeAuthorized fetches the given chunk ids and returns the subset the
// caller's read predicate admits, keyed by id. Batched when the store supports
// it (one round-trip); falls back to the per-id authorizedDoc loop otherwise.
// Authorization semantics are IDENTICAL on both paths: the predicate is applied
// to each returned row's classification tags, and refused/missing rows are
// simply absent.
func materializeAuthorized(ctx context.Context, vs kgExpandVectorSearch, scope *domain.TagPredicate, ids []string) map[string]domain.Document {
	out := make(map[string]domain.Document, len(ids))
	if bf, ok := vs.(batchChunkFetcher); ok {
		docs, err := bf.ChunksByIDs(ctx, ids)
		if err == nil {
			for _, d := range docs {
				if scope.Allows(docTags(d.Metadata)) {
					out[d.ID] = d
				}
			}
			return out
		}
		slog.Warn("materializeAuthorized: batched fetch failed; per-id fallback", "err", err, "ids", len(ids))
	}
	for _, id := range ids {
		if d, ok := authorizedDoc(ctx, vs, scope, id); ok {
			out[id] = d
		}
	}
	return out
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
	// AliasIndex, when non-nil, supplies embedding-nearest entity names for the
	// top frontier entities (query-time synonym linking). Optional.
	AliasIndex aliasNeighborIndex
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
