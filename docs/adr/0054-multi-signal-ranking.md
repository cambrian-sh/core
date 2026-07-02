# ADR-0054: Multi-Signal Ranking with bge-reranker-large (Layer 4)

**Status:** Proposed (2026-06-25) — supersedes the prior "seeds-first / expanded = 0.5 placeholder" ordering in `kgExpand`. Builds on ADR-0053 D3 (KG²RAG retrieval) and the new `(confidence, sources[])` triplet metadata from migration 009.
**Date:** 2026-06-25
**Author:** Afsin
**Depends on:** ADR-0015 (temporal decay), ADR-0017 (spreading activation), ADR-0042 (centralized LLM provider — adds bge-reranker as a new generator role), ADR-0053 (chunks + chunk_triplets + KG²RAG).
**Foundational citations:** KG²RAG (Zhu et al., 2025, arXiv:2502.06864, NAACL 2025) for the chunk-anchored retrieval pattern; bge-reranker-large (Chen et al., 2023, arXiv:2309.07597) for the cross-encoder reranker; PageRank (Page et al., 1999) for the graph-centrality prior.

---

## Context

The current `kgExpand` (`internal/memory/kg_expand.go:53-153`) returns chunks in a fragile order:

1. **Seeds** in their input order (vector search cosine order)
2. **Expanded chunks** in `ChunksMentioningEntity` recency order (the SQL we just fixed), each with a **fixed `Score: 0.5`** placeholder at `kg_expand.go:140`

This single-signal order has three problems:

- **No relevance** — expanded chunks have no vector score; the 0.5 placeholder only "survives" the downstream cosine rerank, it doesn't actually rank.
- **No multi-source agreement** — a triplet produced by both `metadata` and `spacy_patterns` (confidence=2) and one produced only by `spacy_patterns` (confidence=1) rank identically. The new `(confidence, sources[])` from migration 009 is unused at retrieval time.
- **No structural importance** — a chunk that bridges many entities is no better than a chunk that mentions one entity. PageRank / entity-centrality is not computed.

The bge-reranker-large is a cross-encoder that takes `(query, chunk)` pairs and produces a relevance score. It is the highest-accuracy single signal in modern IR (~nDCG@10 improvements of 5-15 points over cosine), but it is also the most expensive (~50ms/pair on GPU, ~500ms on CPU). PageRank is cheap (~5ms for 60k nodes, batch) but query-unaware.

**The lesson:** don't pick one. Compose them. The bge-reranker is the relevance oracle, PageRank is the structural prior, the multi-signal blend is the cheap default. Layer 4 is the composition.

This ADR captures only the **ranking** — the blend formula, the config knobs, the bge-reranker integration, the PageRank computation. The MemoryAnswerAgent (a new system agent that owns the synthesis LLM call) is ADR-0055. Community detection (Layer 3) is ADR-0056.

---

## Decision 1 — A six-signal blend, applied at two stages

The final score is a weighted sum of six signals. The signals are computed in two stages:

```
                                  ┌──────────────────────────┐
  Stage A — pre-rerank (always)   │                          │
                                  │  • cosine (vs query)     │
  Computed for every candidate    │  • recency (extracted_at)│
  chunk in the expansion set.     │  • confidence (0/1/2)    │
  Default blend is cheap.         │  • activation_strength   │
                                  │  • pagerank (graph)      │
                                  │                          │
                                  │  pre_score = sum(w_i)    │
                                  └────────────┬─────────────┘
                                               │   top-K (default 50)
                                               ▼
  Stage B — final rerank (flag)   ┌──────────────────────────┐
                                  │                          │
  Cross-encoder over (query,      │  bge_score = reranker(   │
  chunk) pairs. ~50ms/pair GPU.   │    query, chunk_text)    │
  K tunable.                     │                          │
                                  │  final = w_bge·bge +    │
                                  │    (1 - w_bge)·pre_score │
                                  └────────────┬─────────────┘
                                               │
                                               ▼
                                  ┌──────────────────────────┐
                                  │  top-N (default 10)      │
                                  │  returned to caller      │
                                  └──────────────────────────┘
```

**The default blend (configurable):**

| signal | default weight | source | cost |
|---|---|---|---|
| bge_score (Stage B) | **0.50** | `bge-reranker-large` cross-encoder | ~50ms/pair GPU, K=50 → 2.5s |
| cosine (Stage A) | 0.20 | existing `vectorStore.embedding <=> query_vec` | ~2ms |
| recency (Stage A) | 0.10 | `1 / (1 + days_since_extracted_at)` | ~1ms |
| confidence (Stage A) | 0.10 | `chunk_triplets.confidence` (0/1/2, normalized) | ~1ms |
| pagerank (Stage A) | 0.05 | pre-computed, cached per-corpus | ~0ms at query |
| activation (Stage A) | 0.05 | `documents.activation_strength` | ~1ms |

**Sums to 1.0** by default. Operators tune via `config.json` per deployment.

**The `bge_score` weight (`w_bge`) is the master knob:**
- `w_bge = 0.0` — pure Stage A blend, no GPU dependency, ~5ms total
- `w_bge = 0.5` (default) — balanced, the typical deployment
- `w_bge = 1.0` — pure rerank, ignores Stage A (not recommended; Stage A's `pagerank` and `confidence` carry useful priors)

If the bge-reranker is unreachable (GPU down, OOM, model not loaded), the dispatcher fail-softs to `w_bge = 0.0` and logs a warning. **Retrieval never fails because the reranker is down** — same fail-soft contract as the LLM broker (ADR-0042).

---

## Decision 2 — PageRank over the chunk graph, pre-computed, cached

PageRank treats the chunk graph as a directed graph where:
- **Nodes** = chunks (`documents.id`)
- **Edges** = shared-entity co-occurrence in `chunk_triplets` (a chunk A and chunk B are connected if A has a triplet mentioning entity E and B has a triplet mentioning E)
- **Edge weight** = number of shared entities (or sum of `confidence` over the shared triplets, as a quality-weighted variant)

The PageRank score is a chunk's structural importance in the corpus — a chunk that bridges many entities gets a high score, a chunk that mentions a single obscure entity gets a low score.

**Computation cadence:**
- **Trigger:** every `pageRank_recompute_interval` (default 6h) OR on-demand when the corpus changes by ≥ 5% (chunk count delta).
- **Algorithm:** standard power iteration, 50 iterations, damping=0.85. Implementation in Go (`internal/memory/pagerank.go`).
- **Storage:** a new `chunk_pagerank` table `(chunk_id TEXT PRIMARY KEY, score REAL, computed_at TIMESTAMPTZ)`. Backfilled on first compute.
- **Query-time cost:** a single `chunk_id IN (...)` lookup, ~1ms for 50 candidates.

**Why pre-computed, not on-the-fly:** PageRank over 60k nodes takes ~5 minutes. The graph changes by ~0.1% per ingest. The pre-compute schedule matches the recompute cadence of Cambrian's other derived signals (Hebbian weights, profile aggregations).

**Why chunk-level, not entity-level:** the kgExpand already operates at the chunk level (`ChunksMentioningEntity` returns chunk IDs). Entity-level PageRank would require an extra join. Chunk-level is cheaper and the result feeds the same ranking pipeline.

---

## Decision 3 — bge-reranker-large as a new generator role

bge-reranker-large is a cross-encoder (~560M params). It is registered as a new generator role in Cambrian's LLM provider (ADR-0042):

```go
type GeneratorRole string

const (
    GeneratorRoleLLM       GeneratorRole = "llm"
    GeneratorRoleEmbedding GeneratorRole = "embedding"
    GeneratorRoleReranker  GeneratorRole = "reranker"  // NEW
)
```

Config:
```json
{
  "reranker": {
    "enabled": true,
    "model": "bge-reranker-large",
    "device": "cuda:0",
    "max_length": 512,
    "top_k": 50,
    "weight": 0.5,
    "fail_soft": true
  }
}
```

**The model runs in its own process** — `cmd/rerank-server/` — exposing a small gRPC interface:
```protobuf
service RerankServer {
  rpc Rerank(RerankRequest) returns (RerankResponse);
}
message RerankRequest { repeated string chunks = 1; string query = 2; }
message RerankResponse { repeated float scores = 1; }
```

The Go-side adapter (`internal/llm/rerank_client.go`) implements the `Reranker` port that the new `kgExpand` calls. The model server can be:
- A sidecar container (preferred for production)
- A colocated process on the same GPU as the LLM gateway
- A remote HTTPS endpoint (Cohere Rerank, mixedbread rerank, etc. — the same port)

**Why a separate process:** bge-reranker-large is ~2GB VRAM. Sharing a GPU with the LLM gateway is feasible but the latency tail (LLM can preempt the reranker's forward pass) is unacceptable. Isolating the reranker guarantees sub-100ms p99 for the cross-encoder call.

**Why a port, not a hard dependency:** the LLM broker (ADR-0042) pattern: the reranker is a discoverable capability, not a compile-time dependency. If the config has no reranker, the blend uses `w_bge = 0.0` and the pipeline degrades to Stage A only.

---

## Decision 4 — Integration point in `kgExpand`

The fix is in two places:

### 4.1 — `internal/memory/kg_expand.go:138-141`

The fixed `Score: 0.5` becomes a multi-signal computation:

```go
nextFrontier = append(nextFrontier, domain.SearchResult{
    Document: mustGetDoc(ctx, vectorSearch, id),
    Score:    blend.StageAScore(ctx, query, chunk, blendOpts),  // cosine + recency + conf + pr + act
})
```

`StageAScore` reads the blend weights from config, looks up the chunk's `confidence` (avg over its triplets), its `activation_strength`, and its PageRank score, and returns the weighted sum.

### 4.2 — `internal/memory/kg_expand.go` (new) Stage B

After the expansion budget is consumed, the new Stage B:

```go
func rerankFinal(ctx, query, candidates, reranker, blend) []SearchResult {
    if !blend.StageBEnabled || len(candidates) == 0 { return candidates }
    topK := blend.TopKForRerank
    if len(candidates) > topK { candidates = topNByStageA(candidates, topK) }
    pairs := buildPairs(query, candidates)
    bgeScores, err := reranker.Rerank(ctx, pairs)
    if err != nil { return candidates }  // fail-soft
    for i := range candidates {
        candidates[i].Score = blend.WeightBGE*bgeScores[i] + (1-blend.WeightBGE)*candidates[i].Score
    }
    sort.Slice(candidates, func(i, j int) bool { return candidates[i].Score > candidates[j].Score })
    return candidates
}
```

The result is the final top-N returned to the caller (or to `MemoryAnswerAgent` per ADR-0055).

---

## Decision 5 — Entity ranking by IDF + PageRank (not mention count)

The current `rankEntitiesByCount` (`kg_expand.go:182-207`) ranks entities by mention frequency in the frontier. A new `rankEntitiesByImportance` replaces it:

```
entity_score(e) = α · IDF(e) + β · PageRank_entity(e)
```

- **IDF:** `log(N / df(e))` where `N` is the total chunk count, `df(e)` is the number of chunks mentioning `e`. An entity that appears in many chunks is generic (low IDF); a rare entity is specific (high IDF).
- **PageRank_entity:** PageRank over the entity graph (entities connected via shared chunks). Pre-computed alongside chunk PageRank. An entity that co-occurs with many other entities is central.

Default α=0.6, β=0.4. The 30-entity budget is unchanged.

**Why IDF + PageRank, not just mention count:** a chunk mentioning `caroline` (a specific, central entity) is more useful than a chunk mentioning `ally of` (a generic verb phrase). Mention count would put them in the wrong order.

---

## Considered solutions

### C1 — Cosine only (current behavior)

**Pros:** zero infra change, zero GPU dependency, ~2ms latency.
**Cons:** misses paraphrase, misses multi-source agreement, misses structural importance. The exact failure mode the user hit on LoCoMo: a paraphrased fact has low cosine to the query, even though the LLM-extracted triplet bridges to the answer.

**Rejected:** the gap is structural. A pure-cosine blend is what we have now and it loses to the bge-reranker by 5-15 nDCG@10 points. We can do better without paying the bge cost by adding the cheap signals (recency, confidence, pagerank, activation).

### C2 — bge-reranker-large only

**Pros:** the highest accuracy single signal.
**Cons:** ~2.5s for K=50, GPU dependency, no structural signal. Ignores the cheap priors (recency, confidence, pagerank) that the multi-signal blend gets for free.

**Rejected:** paying the GPU cost without using the cheap priors is wasteful. The blend at `w_bge=0.5` keeps the bge's accuracy gain while preserving the cheap signals as a fail-soft path.

### C3 — PageRank only

**Pros:** zero LLM dependency, batch-computed, query-unaware (good for "what's important in this corpus?").
**Cons:** query-unaware. A query about "support for lgbt" gets the same ranking as a query about "what's the weather". PageRank is a prior, not a relevance signal.

**Rejected:** on its own, PageRank is not a retrieval signal. It is one component of the blend.

### C4 — Multi-signal without bge (cheapest)

**Pros:** no GPU dependency, ~5ms total, ~3-5 nDCG@10 improvement over cosine alone.
**Cons:** misses the relevance gain bge provides.

**Adopted as the fail-soft path at runtime** when bge is unreachable or `w_bge=0.0` (set by config). The dispatcher fail-softs transparently — the caller sees a slightly lower-quality ranking, not an error. This is **not** a separate shipped phase; Layer 4 ships PageRank + bge together in v1, and the fail-soft is a runtime degradation mode, not a development milestone.

### C5 — Multi-signal with bge at top-K (chosen)

**Pros:** bge's accuracy gain + cheap signals as priors + fail-soft to C4.
**Cons:** GPU dependency, K must be tuned.

**Chosen.** The default `w_bge=0.5, K=50` is the recommended starting point; LoCoMo validation will tune the weights and K.

### C6 — LLM-as-reranker (use the LLM to score candidates)

**Pros:** no new model, the same LLM that does the synthesis also reranks.
**Cons:** ~500ms per (query, chunk) call vs ~50ms for bge. At K=50, that's 25 seconds. The LLM's "relevance" is also noisier than bge's (the LLM is biased by the prompt).

**Rejected as a default.** It is the natural fallback if bge is unavailable AND a small model is needed (e.g., a quantized 7B reranker). Not in v1.

---

## Implementation

**Single ship:** PageRank, the multi-signal blend, and the bge-reranker all ship together as one Layer 4 unit. The blend is meaningless without both the structural prior (PageRank) and the relevance oracle (bge); the bge is meaningless without the cheap priors; PageRank is meaningless as a retrieval signal on its own. They are one decision, not three.

### The build (1 week)

1. `db/migrations/010_chunk_pagerank.sql` — new table `(chunk_id TEXT PRIMARY KEY, score REAL, computed_at TIMESTAMPTZ)`.
2. `internal/memory/pagerank.go` — new file. Power-iteration algorithm over the chunk graph; entity PageRank in the same file (Decision 5).
3. `internal/memory/blend.go` — new file. The `StageAScore` function, the `Blender` struct, the `BlendOpts` config, the `rerankFinal` Stage B.
4. `internal/llm/rerank_client.go` — the `Reranker` port and the gRPC adapter.
5. `internal/llm/registry.go` — register `reranker` as a new `GeneratorRole`.
6. `cmd/rerank-server/main.go` — new binary. Loads `bge-reranker-large`, serves the `Rerank` gRPC.
7. `internal/memory/kg_expand.go:138-141` — replace the `Score: 0.5` placeholder with `StageAScore(...)`.
8. `internal/memory/kg_expand.go:182` — replace `rankEntitiesByCount` with `rankEntitiesByImportance` (IDF + PageRank).
9. `internal/memory/kg_expand.go` — new `rerankFinal` Stage B that calls the bge reranker.
10. `cmd/orchestrator/main.go` — wire the `Blender`, the `PageRankStore`, the `RerankClient`, the recompute ticker (every 6h, or 5% corpus delta).
11. `configs/config.json` — add the `blend`, `pagerank`, and `reranker` blocks (Appendix B).

**Estimate: 1 week.**

### Validation on LoCoMo (2 days)

1. Run the existing `benchmarks/locomo/` with `kg_extractor_enabled: true`, `blend.enabled: true`, `reranker.enabled: true`.
2. Sweep the blend weights: `(w_bge, w_cos, w_rec, w_conf, w_pr, w_act)` ∈ a small grid (e.g., 6^3 = 216 combos, run with early stopping).
3. Measure: `nDCG@10`, `MRR`, `graph_recall` (the existing LoCoMo metric), `latency_p99`.
4. Publish the sweep to `benchmarks/locomo/results/0054_multi_signal_ranking.json`.

**Estimate: 2 days.**

---

## Migration plan

1. **Schema migration 010** — `chunk_pagerank` table. Idempotent. Backfill on first compute.
2. **`Blender` constructor** — fails open: if config is missing, blend with `w_bge=0.0` and the cheap signals at default weights.
3. **PageRank recompute ticker** — starts on `MemoryStack.Start`, recomputes every 6h, logs progress.
4. **`UseBlender` on `kgExpand`** — opt-in. The default `kgExpand` is unchanged until the operator enables the blend in config.
5. **Validation** — run LoCoMo in shadow mode (new blend + old blend side-by-side, log both orderings) before flipping the default.

**Rollback:** delete the `chunk_pagerank` table, set `blend.enabled: false`, restart. No data loss; the new ranking is a derived signal, not a source of truth.

---

## Open questions

1. **Top-K for the bge stage.** Default 50 — but K=20 saves 60% of the rerank cost. The LoCoMo sweep will tell us the right K. K should also be query-adaptive: long queries get more candidates.
2. **PageRank damping factor.** Default 0.85 (Google's original). The chunk graph is denser than the web graph; 0.7 may work better. The sweep will tune.
3. **PageRank entity graph vs chunk graph.** We compute both. The entity PageRank feeds `rankEntitiesByImportance`; the chunk PageRank feeds the blend. Both use the same `chunk_triplets` source.
4. **bge vs bge-m3.** bge-m3 is multi-lingual and multi-granularity (8K context). For Cambrian's English-only Substrate, bge-reranker-large is fine. For non-English data, swap to bge-m3.
5. **Cohere Rerank as a remote fallback.** The Reranker port accepts any adapter. A Cohere HTTPS adapter is ~50 lines, gives a fail-soft path if the local bge is down.

---

## Literature anchor

| Claim | Source |
|---|---|
| **Chunk-anchored retrieval with per-chunk triplets** | KG²RAG (Zhu et al., 2025, arXiv:2502.06864) — the foundational citation |
| **Cross-encoder reranking for retrieval** | bge-reranker-large (Chen et al., 2023, arXiv:2309.07597) — the relevance oracle |
| **PageRank as a structural prior in IR** | PageRank (Page et al., 1999) — Stanford InfoLab tech report |
| **Multi-signal blend for retrieval ranking** | ColBERT v2 (Santhanam et al., 2022, arXiv:2205.01107) — late-interaction + cross-encoder rerank |
| **Confidence-weighted provenance in IR** | KG²RAG §3.2 + Cambrian's own (confidence, sources[]) from migration 009 |
| **Fail-soft LLM broker pattern** | Cambrian ADR-0042 (centralized LLM provider) — the bge follows the same pattern |

---

## Appendix A — The blend formula

```
final_score(c) = w_bge · bge(query, c) 
               + w_cos · cos(query, c.embedding)
               + w_rec · 1 / (1 + days_since(c.max_extracted_at))
               + w_conf · mean(c.triplets.confidence) / 2
               + w_pr  · chunk_pagerank[c.id]
               + w_act · c.activation_strength
```

Where:
- `bge(query, c)` ∈ [0, 1] — the cross-encoder score
- `cos(query, c.embedding)` ∈ [0, 1] — cosine similarity
- `days_since(c.max_extracted_at)` — the most recent `extracted_at` of any triplet on the chunk
- `mean(c.triplets.confidence) / 2` — averages the confidence over all triplets, normalized to [0, 1]
- `chunk_pagerank[c.id]` ∈ [0, 1] — pre-computed PageRank
- `c.activation_strength` ∈ [0, 1] — Cambrian's existing activation field

All weights are tunable. The default `(0.5, 0.2, 0.1, 0.1, 0.05, 0.05)` sums to 1.0.

## Appendix B — The config schema

```json
{
  "blend": {
    "enabled": true,
    "stage_b": {
      "enabled": true,
      "weight": 0.5,
      "top_k": 50,
      "reranker": {
        "type": "bge-reranker-large",
        "address": "localhost:50060",
        "timeout_ms": 100,
        "fail_soft": true
      }
    },
    "stage_a": {
      "weight_cosine": 0.2,
      "weight_recency": 0.1,
      "weight_confidence": 0.1,
      "weight_pagerank": 0.05,
      "weight_activation": 0.05
    },
    "entity_ranking": {
      "weight_idf": 0.6,
      "weight_pagerank": 0.4,
      "max_entities": 30
    }
  },
  "pagerank": {
    "damping": 0.85,
    "iterations": 50,
    "recompute_interval_min": 360,
    "trigger_recompute_on_delta_pct": 5
  }
}
```

## Appendix C — What stays the same

- **D1 chunks schema** — unchanged.
- **D2 per-chunk triplets** — unchanged. The new `(confidence, sources[])` columns from migration 009 are now consumed at retrieval time.
- **D3 KG²RAG pipeline shape** — unchanged. The five stages (vector → kgExpand → graph filter → spreader → rerank) stay; only Stage 5 is upgraded to a multi-signal blend and Stage 5.5 (the bge rerank) is added.
- **Hex invariant** — preserved. The retrieval pipeline is deterministic reflex; the LLM (or bge) is an opt-in residue / oracle.
- **Zero-Hardcode Rule** — preserved. The blend weights are config, not code. Operators tune per deployment.
