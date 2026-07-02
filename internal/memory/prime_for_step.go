package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"sort"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// initCaches initialises the LRU caches with the given capacity.
// Called at construction time or by tests. Safe to call multiple times.
func (ws *WorkspaceStageImpl) initCaches(capacity int) {
	ws.cacheMu.Lock()
	defer ws.cacheMu.Unlock()
	ws.refCache = newLRUCache[string, []domain.ContextRef](capacity)
	ws.embedCache = newLRUCache[string, []float32](capacity)
}

// InvalidateContextRefCache clears the query→[]ContextRef LRU cache.
// Called by MemoryAgent after every Tier-2 pgvector drain — new documents
// committed to pgvector could change which refs PrimeForStep selects.
// The embedding cache is NOT cleared (embeddings are deterministic).
func (ws *WorkspaceStageImpl) InvalidateContextRefCache() {
	ws.cacheMu.Lock()
	defer ws.cacheMu.Unlock()
	if ws.refCache != nil {
		ws.refCache.Clear()
	}
}

// PrimeForStep selects a capacity-limited working set for a single step dispatch.
// ADR-0022 Phase 2.
//
// Pipeline:
//  1. Embed step query (embedding cache: TTL-free, deterministic)
//  2. pgvector ANN seeds (ref cache: invalidated on Tier-2 drain)
//  3. SpreadingEngine.Spread → BFS expansions (nil-safe: seeds-only fallback)
//  4. DependsOn activation boost (+0.3, clamped to 1.0)
//  5. Activation threshold filter (NOT RetrievalFloor — different scales)
//  6. Hard ceiling at maxItems; sort by activation descending
//
// Precision semantics:
//   - Seeds (pgvector hits): Precision = cosine score
//   - BFS-discovered nodes: Precision = -1.0 (sentinel: "not yet computed")
func (ws *WorkspaceStageImpl) PrimeForStep(
	ctx context.Context,
	query string,
	priorStepRefs []domain.ContextRef,
	planningFacts []domain.SearchResult,
	stepFactCosineThreshold float64,
	maxItems int,
) ([]domain.ContextRef, error) {

	// Fast path: check ref cache first.
	// The cache is keyed only on the query string, not on priorStepRefs —
	// DependsOn boosts are applied on top of the cached base result.
	// This is intentional: the base retrieval (pgvector + BFS) is query-driven;
	// the boost is a small additive adjustment that doesn't justify a cache miss.
	// AGENTCONTEXTREQ: planning facts are NOT cached because their relevance
	// depends on the per-step query; they are always re-filtered.
	ws.cacheMu.Lock()
	initIfNil(ws)
	cachedBase, cacheHit := ws.refCache.Get(query)
	ws.cacheMu.Unlock()

	// Embedding: check embedding cache (TTL-free).
	ws.cacheMu.Lock()
	cachedVec, vecHit := ws.embedCache.Get(query)
	ws.cacheMu.Unlock()

	var queryVec []float32
	if vecHit {
		queryVec = cachedVec
	} else {
		var err error
		queryVec, err = ws.Embedder.Embed(ctx, query)
		if err != nil {
			return nil, err
		}
		ws.cacheMu.Lock()
		ws.embedCache.Put(query, queryVec)
		ws.cacheMu.Unlock()
	}

	var base []domain.ContextRef
	if cacheHit {
		base = cachedBase
	} else {
		// pgvector ANN seed search.
		seeds, err := ws.Store.Search(ctx, queryVec, domain.SearchOptions{
			TopK:           20,
			RetrievalFloor: ws.RetrievalFloor,
			Scope:          domain.ScopeSystem, // ADR-0034: workspace priming is a kernel-internal read
		})
		if err != nil {
			return nil, err
		}
		// ADR-0048 D3 (defensive): PrimeForStep is unwired, but if it is ever
		// re-wired its ScopeSystem seed search must not re-inject the current run's
		// own step records — that would re-open the D1 recall loop via the workspace.
		if sid, ok := domain.SessionIDFromContext(ctx); ok {
			seeds = excludeSameSessionStepRecords(seeds, sid)
		}

		// BFS expansion (nil-safe).
		var expansions []domain.GraphNodeExpansion
		if ws.SpreadingEngine != nil {
			expansions = ws.SpreadingEngine.Spread(ctx, seeds)
		} else {
			expansions = make([]domain.GraphNodeExpansion, len(seeds))
			for i, s := range seeds {
				expansions[i] = domain.GraphNodeExpansion{
					Document:         s.Document,
					ActivationEnergy: s.Score,
				}
			}
		}

		// Build seed precision index.
		seedPrecision := make(map[string]float64, len(seeds))
		for _, s := range seeds {
			seedPrecision[s.Document.ID] = s.Score
		}

		// Build base refs (no boost applied — cached independently of priorStepRefs).
		// snippetChars: use 500 (ADR-0022 default). A WorkspaceStageImpl.SnippetChars field
		// can be added later to expose this via config; 500 is correct per ADR-0022 §5 Flaw 10.
		const snippetChars = 500
		base = make([]domain.ContextRef, 0, len(expansions))
		for _, exp := range expansions {
			precision := float32(-1.0)
			if p, ok := seedPrecision[exp.Document.ID]; ok {
				precision = float32(p)
			}
			base = append(base, domain.ContextRef{
				CID:        domain.CID(exp.Document.ID),
				Type:       exp.Document.DocumentType,
				Activation: float32(exp.ActivationEnergy),
				Precision:  precision,
				Snippet:    textSnippet(exp.Document.Text, snippetChars),
			})
		}

		// Cache the base result (before boost).
		ws.cacheMu.Lock()
		ws.refCache.Put(query, base)
		ws.cacheMu.Unlock()
	}

	// AGENTCONTEXTREQ REQ2-3: filter planning facts by per-step cosine similarity
	// and merge them ahead of speculative BFS results.
	merged := mergePlanningFacts(base, planningFacts, queryVec, stepFactCosineThreshold)

	result := applyBoostAndFilter(merged, priorStepRefs, ws.activationThreshold(), maxItems)

	bfsCount := 0
	for _, r := range result {
		if r.Precision == -1.0 {
			bfsCount++
		}
	}
	bfsFraction := float64(0)
	if len(result) > 0 {
		bfsFraction = float64(bfsCount) / float64(len(result))
	}
	slog.Info("workspace_prime_for_step",
		"seeds_returned", len(base),
		"planning_facts", len(planningFacts),
		"selected_count", len(result),
		"truncated", maxItems > 0 && len(result) == maxItems,
		"bfs_fraction", bfsFraction,
		"cache_hit", cacheHit,
	)

	return result, nil
}

// activationThreshold returns the configured threshold, defaulting to 0.1.
func (ws *WorkspaceStageImpl) activationThreshold() float64 {
	if ws.ActivationThreshold > 0 {
		return ws.ActivationThreshold
	}
	return 0.1
}

// initIfNil ensures caches are initialised even if initCaches was never called.
// Caller must hold cacheMu. Uses LRUCacheCapacity when set, else falls back to 100.
func initIfNil(ws *WorkspaceStageImpl) {
	cap := ws.LRUCacheCapacity
	if cap <= 0 {
		cap = 100
	}
	if ws.refCache == nil {
		ws.refCache = newLRUCache[string, []domain.ContextRef](cap)
	}
	if ws.embedCache == nil {
		ws.embedCache = newLRUCache[string, []float32](cap)
	}
}

// textSnippet returns the first n runes of s. Returns "" for empty or non-UTF-8
// content — a garbled snippet (binary prefix) is worse than no snippet at all.
func textSnippet(s string, n int) string {
	if s == "" || n <= 0 {
		return ""
	}
	count := 0
	for i := range s {
		if count >= n {
			return s[:i]
		}
		count++
	}
	return s
}

// mergePlanningFacts filters planning-time facts by per-step cosine similarity
// and prepends them to the base refs. AGENTCONTEXTREQ REQ2-3.
// Planning facts are pre-validated at MinFactCosine=0.60; here we apply a
// slightly lower per-step threshold (default 0.55) because step queries are
// narrower than the full user request.
func mergePlanningFacts(
	base []domain.ContextRef,
	planningFacts []domain.SearchResult,
	queryVec []float32,
	threshold float64,
) []domain.ContextRef {
	if len(planningFacts) == 0 || len(queryVec) == 0 {
		return base
	}
	if threshold <= 0 {
		threshold = 0.55
	}

	const snippetChars = 500
	seenCID := make(map[domain.CID]struct{}, len(base))
	seenHash := make(map[string]struct{}, len(base))
	for _, r := range base {
		seenCID[r.CID] = struct{}{}
		h := sha256.Sum256([]byte(r.Snippet))
		seenHash[hex.EncodeToString(h[:])] = struct{}{}
	}

	var filtered []domain.ContextRef
	for _, f := range planningFacts {
		if len(f.Document.Embedding.Vector) == 0 {
			continue
		}
		sim := cosineSimilarity(queryVec, f.Document.Embedding.Vector)
		if sim < threshold {
			continue
		}
		cid := domain.CID(f.Document.ID)
		if _, ok := seenCID[cid]; ok {
			continue // deduplicate by document ID
		}
		// REQ-DEDUP-3: also deduplicate by content hash
		h := sha256.Sum256([]byte(f.Document.Text))
		hashStr := hex.EncodeToString(h[:])
		if _, ok := seenHash[hashStr]; ok {
			continue
		}
		seenHash[hashStr] = struct{}{}
		filtered = append(filtered, domain.ContextRef{
			CID:        cid,
			Type:       f.Document.DocumentType,
			Activation: float32(f.Document.ActivationStrength),
			Precision:  float32(sim), // per-step relevance
			Snippet:    textSnippet(f.Document.Text, snippetChars),
		})
	}

	// Prepend planning facts (pre-validated) before speculative BFS results.
	return append(filtered, base...)
}

// applyBoostAndFilter applies DependsOn activation boosts, threshold filter,
// hard ceiling, and activation-descending sort to a base []ContextRef slice.
// Returns a new slice — does not mutate the cached base.
//
// ADR-0022 Phase 3 fix: priorStepRefs (direct dependency outputs from the
// ContentStore) are injected into the output even when their CID is not present
// in the pgvector base. This ensures inter-step context never gets lost.
func applyBoostAndFilter(
	base []domain.ContextRef,
	priorStepRefs []domain.ContextRef,
	threshold float64,
	maxItems int,
) []domain.ContextRef {
	priorBoost := make(map[domain.CID]float32, len(priorStepRefs))
	for _, ref := range priorStepRefs {
		priorBoost[ref.CID] += 0.3
	}

	out := make([]domain.ContextRef, 0, len(base))
	seen := make(map[domain.CID]struct{}, len(base))
	for _, ref := range base {
		seen[ref.CID] = struct{}{}
		energy := float64(ref.Activation) + float64(priorBoost[ref.CID])
		if energy > 1.0 {
			energy = 1.0
		}
		if energy < threshold {
			continue
		}
		boosted := ref
		boosted.Activation = float32(energy)
		if priorBoost[ref.CID] > 0 {
			boosted.Precision = 1.0 // direct dependency = highest relevance
		}
		out = append(out, boosted)
	}

	// Inject any priorStepRefs whose CID was NOT in the pgvector base.
	// These are direct step outputs from the ContentStore and must be
	// visible to downstream steps regardless of LTM retrieval overlap.
	for _, ref := range priorStepRefs {
		if _, ok := seen[ref.CID]; ok {
			continue
		}
		injected := ref
		if injected.Activation == 0 {
			injected.Activation = 1.0 // direct dependency = highest relevance
		}
		injected.Precision = 1.0
		out = append(out, injected)
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].Activation > out[j].Activation
	})

	// Protect direct dependencies (priorStepRefs) from truncation — they must
	// always be visible to downstream steps. Move them to the front before
	// applying the hard ceiling.
	if len(priorStepRefs) > 0 {
		priorCIDs := make(map[domain.CID]struct{}, len(priorStepRefs))
		for _, ref := range priorStepRefs {
			priorCIDs[ref.CID] = struct{}{}
		}
		// Stable partition: priorCIDs first, then everything else.
		var front, back []domain.ContextRef
		for _, ref := range out {
			if _, ok := priorCIDs[ref.CID]; ok {
				front = append(front, ref)
			} else {
				back = append(back, ref)
			}
		}
		out = append(front, back...)
	}

	if maxItems > 0 && len(out) > maxItems {
		slog.Info("workspace_capacity_truncated",
			"kept", maxItems,
			"total_before_cut", len(out),
			"activation_min_kept", out[maxItems-1].Activation,
			"activation_max_dropped", out[maxItems].Activation,
		)
		out = out[:maxItems]
	}
	return out
}
