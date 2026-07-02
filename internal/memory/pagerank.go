package memory

import (
	"context"
	"math"
	"sort"
	"time"
)

// PageRank over the chunk graph (ADR-0054 D2). The graph is implicit in
// chunk_triplets: two chunks are connected if they share an entity (a triplet
// head or tail). A chunk that bridges many entities scores high; a chunk that
// mentions one obscure entity scores low. The score is a query-UNAWARE
// structural prior — one of several signals the Stage-A blend combines.
//
// This file is PURE (no DB, no I/O) so the algorithm is unit-testable with
// synthetic graphs and swappable behind the PageRankComputer seam (a future
// gonum / scipy-sidecar adapter implements the same interface). The recompute
// WORKER (cmd/pagerank-recompute) feeds it chunk_triplets and persists the
// result; the kernel only READS the persisted table.

// ChunkEntities is one chunk and the entities it mentions (the deduped union of
// its triplet heads + tails, lowercased). The input unit for ComputePageRank.
type ChunkEntities struct {
	ChunkID  string
	Entities []string
}

// PageRankParams configures the computation. Zero values get sane defaults.
type PageRankParams struct {
	Damping    float64 // teleport probability complement; default 0.85
	Iterations int     // power-iteration rounds; default 50
	Tolerance  float64 // early-stop when L1 delta < tol; default 1e-6
	// DFCap drops entities mentioned in MORE than DFCap chunks BEFORE building
	// edges. A high-frequency entity (a speaker name, a generic verb phrase)
	// otherwise forms a near-complete subgraph — df=k contributes k*(k-1)/2
	// edges, so an entity in 5,000 chunks is ~12M edges by itself. High-df
	// entities are also low-IDF (generic), so dropping them costs little signal
	// and bounds graph construction to O(sum over kept entities of df^2). 0 ⇒ no cap.
	DFCap int
}

func (p PageRankParams) withDefaults() PageRankParams {
	if p.Damping <= 0 || p.Damping >= 1 {
		p.Damping = 0.85
	}
	if p.Iterations <= 0 {
		p.Iterations = 50
	}
	if p.Tolerance <= 0 {
		p.Tolerance = 1e-6
	}
	return p
}

// PageRankComputer is the seam: the pure-Go power iteration is the v1 adapter;
// a gonum or scipy-sidecar adapter can replace it without touching the worker
// or the store, if a real corpus ever outgrows the in-process implementation.
type PageRankComputer interface {
	Compute(chunks []ChunkEntities, params PageRankParams) map[string]float64
}

// GoPageRank is the pure-Go, in-process PageRankComputer (the v1 default).
type GoPageRank struct{}

func (GoPageRank) Compute(chunks []ChunkEntities, params PageRankParams) map[string]float64 {
	return ComputePageRank(chunks, params)
}

// ComputePageRank builds the df-capped shared-entity chunk graph and runs power
// iteration. Returns chunk_id -> score in [0,1], summing to ~1 over all chunks
// (a probability distribution — directly usable as a blend signal). An empty
// input returns an empty map; isolated chunks (no surviving shared entity) get
// the teleport baseline (1-d)/N.
func ComputePageRank(chunks []ChunkEntities, params PageRankParams) map[string]float64 {
	params = params.withDefaults()
	n := len(chunks)
	out := make(map[string]float64, n)
	if n == 0 {
		return out
	}
	// Stable node index. Dedup chunk ids (defensive); first wins.
	ids := make([]string, 0, n)
	idx := make(map[string]int, n)
	for _, c := range chunks {
		if c.ChunkID == "" {
			continue
		}
		if _, ok := idx[c.ChunkID]; ok {
			continue
		}
		idx[c.ChunkID] = len(ids)
		ids = append(ids, c.ChunkID)
	}
	n = len(ids)
	if n == 0 {
		return out
	}
	if n == 1 {
		out[ids[0]] = 1.0
		return out
	}

	adj := buildChunkAdjacency(chunks, idx, params.DFCap)

	d := params.Damping
	base := (1 - d) / float64(n)
	score := make([]float64, n)
	next := make([]float64, n)
	for i := range score {
		score[i] = 1.0 / float64(n)
	}
	// Out-strength per node (sum of its edge weights). Zero ⇒ dangling.
	outStrength := make([]float64, n)
	for i := range adj {
		var s float64
		for _, w := range adj[i] {
			s += w
		}
		outStrength[i] = s
	}

	for iter := 0; iter < params.Iterations; iter++ {
		// Dangling mass (nodes with no edges) is redistributed uniformly so the
		// scores stay a proper distribution (the standard PageRank correction).
		var dangling float64
		for i := 0; i < n; i++ {
			if outStrength[i] == 0 {
				dangling += score[i]
			}
		}
		danglingShare := d * dangling / float64(n)

		for i := 0; i < n; i++ {
			next[i] = base + danglingShare
		}
		// Push each node's mass along its weighted edges.
		for i := 0; i < n; i++ {
			if outStrength[i] == 0 || score[i] == 0 {
				continue
			}
			contrib := d * score[i] / outStrength[i]
			for j, w := range adj[i] {
				next[j] += contrib * w
			}
		}

		var delta float64
		for i := 0; i < n; i++ {
			delta += math.Abs(next[i] - score[i])
			score[i] = next[i]
		}
		if delta < params.Tolerance {
			break
		}
	}

	for i, id := range ids {
		out[id] = score[i]
	}
	return out
}

// buildChunkAdjacency builds the weighted, symmetric chunk↔chunk adjacency from
// shared entities, after dropping entities with df > DFCap. Edge weight =
// number of shared entities between the two chunks. Returned as adj[i][j]=w.
func buildChunkAdjacency(chunks []ChunkEntities, idx map[string]int, dfCap int) []map[int]float64 {
	n := len(idx)
	// Invert to entity -> set of chunk indices (deduped).
	entityChunks := make(map[string]map[int]struct{})
	for _, c := range chunks {
		ci, ok := idx[c.ChunkID]
		if !ok {
			continue
		}
		seen := make(map[string]struct{}, len(c.Entities))
		for _, e := range c.Entities {
			if e == "" {
				continue
			}
			if _, dup := seen[e]; dup {
				continue
			}
			seen[e] = struct{}{}
			set := entityChunks[e]
			if set == nil {
				set = make(map[int]struct{})
				entityChunks[e] = set
			}
			set[ci] = struct{}{}
		}
	}

	adj := make([]map[int]float64, n)
	for _, members := range entityChunks {
		df := len(members)
		if df < 2 {
			continue // an entity in one chunk creates no edge
		}
		if dfCap > 0 && df > dfCap {
			continue // generic high-df entity: skip (bounds graph size)
		}
		// Materialize the member list, add +1 weight to every pair.
		list := make([]int, 0, df)
		for ci := range members {
			list = append(list, ci)
		}
		sort.Ints(list) // determinism
		for a := 0; a < len(list); a++ {
			for b := a + 1; b < len(list); b++ {
				i, j := list[a], list[b]
				if adj[i] == nil {
					adj[i] = make(map[int]float64)
				}
				if adj[j] == nil {
					adj[j] = make(map[int]float64)
				}
				adj[i][j]++
				adj[j][i]++
			}
		}
	}
	return adj
}

// ── Recompute orchestration (driven by the worker, not the kernel) ──────────

// PageRankSource reads the corpus the graph is built from. Implemented by the
// postgres adapter; the worker depends on this interface, not the DB.
type PageRankSource interface {
	// LoadChunkEntities returns every chunk with its triplet entities (h+t).
	LoadChunkEntities(ctx context.Context) ([]ChunkEntities, error)
	// CorpusStats returns (distinct chunk count, total triplet rows) cheaply.
	CorpusStats(ctx context.Context) (chunkCount, tripletCount int, err error)
}

// PageRankStore persists/reads the computed scores. SaveChunkPageRanks is a
// full replace (the score is a derived snapshot, not an accumulator).
type PageRankStore interface {
	SaveChunkPageRanks(ctx context.Context, scores map[string]float64, chunkCount, tripletCount int) error
	ChunkPageRanks(ctx context.Context, ids []string) (map[string]float64, error)
	PageRankMeta(ctx context.Context) (computedAt time.Time, chunkCount, tripletCount int, err error)
}

// RecomputeAndStore loads the corpus, computes PageRank, and replaces the stored
// scores in one pass. Returns the number of scored chunks. The worker calls this
// on its ticker (and/or on a corpus-delta trigger via ShouldRecompute).
func RecomputeAndStore(ctx context.Context, src PageRankSource, store PageRankStore, computer PageRankComputer, params PageRankParams) (int, error) {
	chunks, err := src.LoadChunkEntities(ctx)
	if err != nil {
		return 0, err
	}
	chunkCount, tripletCount, _ := src.CorpusStats(ctx) // best-effort meta
	scores := computer.Compute(chunks, params)
	if chunkCount == 0 {
		chunkCount = len(scores)
	}
	if err := store.SaveChunkPageRanks(ctx, scores, chunkCount, tripletCount); err != nil {
		return 0, err
	}
	return len(scores), nil
}

// ShouldRecompute decides whether the worker should run now: never computed,
// OR older than maxAge, OR the chunk count moved by >= deltaPct since the last
// compute. Keeps an always-up worker from rebuilding an unchanged graph every tick.
func ShouldRecompute(prevComputed time.Time, prevChunks, curChunks int, deltaPct float64, maxAge time.Duration) bool {
	if prevComputed.IsZero() || prevChunks == 0 {
		return true
	}
	if maxAge > 0 && time.Since(prevComputed) >= maxAge {
		return true
	}
	delta := math.Abs(float64(curChunks-prevChunks)) / float64(prevChunks) * 100
	return delta >= deltaPct
}

// ChunkEntitiesFromTriplets folds a flat (chunk_id, entity) row stream — the
// shape the worker reads from chunk_triplets (head + tail as separate rows) —
// into the per-chunk []ChunkEntities ComputePageRank wants. Entities are taken
// verbatim (the caller lowercases; chunk_triplets stores lowercase).
func ChunkEntitiesFromTriplets(rows []struct{ ChunkID, Entity string }) []ChunkEntities {
	order := make([]string, 0)
	byChunk := make(map[string]map[string]struct{})
	for _, r := range rows {
		if r.ChunkID == "" || r.Entity == "" {
			continue
		}
		set := byChunk[r.ChunkID]
		if set == nil {
			set = make(map[string]struct{})
			byChunk[r.ChunkID] = set
			order = append(order, r.ChunkID)
		}
		set[r.Entity] = struct{}{}
	}
	out := make([]ChunkEntities, 0, len(order))
	for _, id := range order {
		ents := make([]string, 0, len(byChunk[id]))
		for e := range byChunk[id] {
			ents = append(ents, e)
		}
		sort.Strings(ents)
		out = append(out, ChunkEntities{ChunkID: id, Entities: ents})
	}
	return out
}
