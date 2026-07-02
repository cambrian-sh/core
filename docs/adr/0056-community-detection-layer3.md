# ADR-0056: Community Detection for Global Queries (Layer 3) — Discussion

**Status:** Discussion (2026-06-25) — Layer 3 of ADR-0053 D5. Not yet proposed. This ADR explores the design space, lists the considered solutions, and proposes three implementation paths with their trade-offs. The user picks one; the chosen path becomes a follow-up Proposed ADR.
**Date:** 2026-06-25
**Author:** Afsin
**Depends on:** ADR-0053 (chunks + chunk_triplets + KG²RAG), ADR-0054 (multi-signal ranking — community summaries are a derived signal, like PageRank).
**Foundational citations:** Microsoft GraphRAG (Edge et al., 2024, arXiv:2404.16130) — the canonical reference; Leiden algorithm (Traag et al., 2019, Scientific Reports 9) — the community-detection algorithm; BERTopic (Grootendorst, 2022) — the embedding-clustering alternative; Top2Vec (Angelov, 2020, arXiv:2008.09470) — the topic-modeling alternative.

---

## Context

ADR-0053 D3 (KG²RAG retrieval) handles **local queries** — "What did Caroline research?", "When did Melanie go to the park?" — well. The retrieval walks from the seed chunks, follows entity links, reranks, and returns 10-20 chunks. The agent (or `MemoryAnswerAgent` from ADR-0055) synthesizes an answer.

But **global queries** are hard:
- "What are the main themes in my conversations about family?"
- "Summarize the last 6 months of my work life."
- "What are the recurring topics in Caroline's diary?"

These questions are about the **corpus as a whole**, not about a specific fact. The KG²RAG pattern doesn't help — there's no "seed" for "themes". The SpreadingEngine (BFS over `document_edges`) doesn't help either — the document graph is too sparse and the BFS explodes combinatorially.

The right primitive for global queries is **community detection**: cluster the corpus into groups of related chunks, summarize each cluster, and let the LLM answer the question from the cluster summaries. This is the Microsoft GraphRAG pattern.

**The question is: how to do it on Cambrian's chunk-anchored graph?**

Cambrian's `chunk_triplets` is a chunk-level KG, not an entity-level graph. There is no `entities` table. The communities could be:
- **Chunks** (group chunks that share entities)
- **Entities** (group entities that co-occur in chunks)
- **Both** (a hierarchical Leiden over the chunk graph, then over the entity graph)

Each choice has different costs, different retrieval latencies, and different summarization prompts. This ADR explores the design space, lists five considered solutions, and proposes three implementation paths.

---

## The design space

Three orthogonal axes define the solutions:

**Axis 1 — What gets clustered?**
- (a) Chunks (group similar chunks)
- (b) Entities (group related entities)
- (c) Both (hierarchical: chunks first, then entities within clusters)

**Axis 2 — How are clusters formed?**
- (i) Leiden (graph-based, requires a graph)
- (ii) Embedding clustering (HDBSCAN, k-means on chunk/entity embeddings)
- (iii) LLM-only (no clustering, just ask the LLM to summarize)

**Axis 3 — How are clusters summarized?**
- (α) LLM-generated summaries (Microsoft GraphRAG style)
- (β) Centroid chunk (the most-central chunk in the cluster)
- (γ) Triplet aggregation (the most-frequent entities and relations)

The five considered solutions are points in this space. The three proposed paths pick specific (axis 1, axis 2, axis 3) combinations.

---

## Considered solutions

### S1 — Microsoft GraphRAG (canonical)

**Axes:** (b) Entities, (i) Leiden, (α) LLM summaries.

The Microsoft pattern:
1. **Entity extraction** — for every chunk, the LLM extracts entities (people, places, concepts) with descriptions.
2. **Entity graph** — build a graph where nodes are entities, edges are co-occurrences in chunks.
3. **Leiden** — cluster the entity graph into communities.
4. **Hierarchical Leiden** — repeat Leiden at multiple resolutions, producing a tree of communities.
5. **LLM summarization** — for each community, the LLM generates a summary from the entity descriptions.
6. **Query-time** — match the question to a community, return the community summary.

**Pros:**
- The canonical reference. Battle-tested. Microsoft ships it open source.
- Hierarchical Leiden is the right primitive for "drill down" queries ("what about Q3 specifically?").
- Entity-level clusters are more interpretable than chunk-level.

**Cons:**
- **Entity extraction is an LLM call per chunk.** At 60k chunks, that's 60k LLM calls. Even at $0.001/call, $60 per corpus. At 600k chunks, $600.
- **The entity graph is large.** 60k chunks × ~5 entities/chunk = 300k entity nodes. Leiden on 300k nodes takes ~10 minutes.
- **Drift from the existing chunk-level KG.** Cambrian's `chunk_triplets` already encodes `(h, r, t)` triples. The Microsoft entity graph is a parallel structure that must be kept in sync. Two KG layers, two pipelines, two extractors.
- **Cost is per-corpus, not per-query.** Recomputing the entity graph on a 5% corpus change is expensive.

**Verdict:** powerful but heavy. Not appropriate as the v1 Layer 3 on a Cambrian-shaped corpus (chunk-anchored, not entity-anchored).

### S2 — Leiden on the chunk graph (lightweight)

**Axes:** (a) Chunks, (i) Leiden, (α) LLM summaries.

The Cambrian-shaped adaptation:
1. **Chunk graph** — already implicit in `chunk_triplets`. Nodes = chunks. Edges = shared-entity co-occurrence. Edge weight = number of shared entities (or sum of `confidence` over the shared triplets).
2. **Leiden** — cluster the chunk graph into communities. Run at multiple resolutions.
3. **LLM summarization** — for each community, the LLM generates a summary from the member chunks.

**Pros:**
- **Reuses the existing chunk-level KG.** No parallel entity graph, no entity extraction. The chunk graph is implicit in `chunk_triplets` (the same triplets that power `kg_expand.go`).
- **Cheap to compute.** Leiden on 60k nodes takes ~30 seconds. Reusable on 5% corpus delta.
- **No LLM cost for clustering.** The clustering is purely graph-based. The LLM is only used for summarization (one call per community, typically ~100 communities = 100 calls = $0.10 per corpus).
- **Natural fit with ADR-0054.** The PageRank scores from the multi-signal blend are a by-product; the Leiden partition is a sibling by-product.
- **Reuses the existing MemoryAnswerAgent (ADR-0055).** The community summary is just a `synthesis_request` Handoff with the member chunks as context.

**Cons:**
- **Chunk-level clusters are less interpretable than entity-level.** A "community" of 200 chunks that mention `caroline` is "the Caroline cluster" only by virtue of the central entity. The summary prompt has to bridge from chunks to themes.
- **Cluster granularity is the chunk graph's granularity.** If a theme spans 50 chunks but they're all in different communities, the summary misses it. Hierarchical Leiden partially fixes this.
- **Cold-start.** A new corpus with few chunks has noisy clusters. The summarization prompt can return "insufficient data".

**Verdict:** the natural v1. Cheap, reusable, fits Cambrian's chunk-anchored model. The summary prompt does the work of "what's the theme here?".

### S3 — BERTopic / Top2Vec (embedding clustering)

**Axes:** (a) Chunks, (ii) Embedding clustering, (γ) Triplet aggregation.

Unsupervised topic modeling:
1. **Embed every chunk** — the chunk embedding is already in `documents.embedding`.
2. **UMAP → HDBSCAN** (BERTopic) or **Doc2Vec + k-means** (Top2Vec) — cluster the embeddings into topics.
3. **Topic representation** — for each cluster, the centroid embedding is the topic. The most-frequent entities (from `chunk_triplets`) are the topic keywords.

**Pros:**
- **No LLM cost at all.** Pure embedding + clustering. BERTopic is a small library.
- **Embedding-based similarity** captures semantic themes that entity co-occurrence misses.
- **Pluggable.** BERTopic is a Python library, easy to A/B.

**Cons:**
- **No LLM-generated summaries.** The "summary" of a cluster is its centroid embedding or top-N keywords. Less interpretable than an LLM-generated paragraph.
- **UMAP is stochastic.** Re-runs produce different clusters. Reproducibility is hard.
- **Clustering quality is corpus-dependent.** A corpus with many overlapping themes (LoCoMo: family + work + health + travel) has fuzzy clusters. A corpus with one theme has one giant cluster.
- **Doesn't use the KG.** All the `(h, r, t)` triplets we extract are wasted; the clustering only sees the embedding. This is the opposite of S1/S2.

**Verdict:** complementary to S2, not a replacement. The embeddings + KG together is more informative than either alone.

### S4 — LLM-only summarization (no clustering)

**Axes:** (a) Chunks, (iii) LLM-only, (α) LLM summaries.

The simplest approach:
1. **Sample** — for a global query, sample 100-500 representative chunks (by PageRank, by random, or by time-period buckets).
2. **LLM summarize** — feed the sampled chunks to the LLM with a "what are the main themes?" prompt.

**Pros:**
- **Zero infra.** Just an LLM call with a clever prompt.
- **Flexible.** The prompt can be "themes", "timeline", "top entities", "open questions" — different shapes for different queries.
- **Cheap at small scale.** 500 chunks × $0.001/1k tokens = $0.50 per query.

**Cons:**
- **Expensive at scale.** Recomputing on every query is $0.50 × N queries. Caching is necessary.
- **Sampling bias.** The 500 chunks aren't representative of the whole corpus; the summary is biased.
- **No determinism.** Same corpus, same query, different LLM temperatures → different summaries.
- **No drill-down.** "Tell me more about theme X" requires re-sampling and re-summarizing.

**Verdict:** useful for ad-hoc exploration, not a Layer 3 system. The Cambrian existing `cmd/chunk-fill/` already has the primitives; this is just a different prompt.

### S5 — Hybrid: Leiden on chunk graph + LLM community summaries + entity-level drill-down

**Axes:** (c) Both, (i) Leiden, (α) LLM summaries.

The "do everything" path:
1. **Chunk-level Leiden** — partition chunks into communities (S2).
2. **Entity-level Leiden** — within each chunk community, partition the entities (mentioned in those chunks) into sub-communities.
3. **Hierarchical LLM summaries** — summarize each entity sub-community, then summarize the chunk community from the sub-community summaries.
4. **Drill-down** — "what about the Q3 sub-theme?" maps to an entity sub-community.

**Pros:**
- **Most powerful.** Hierarchical summaries match the natural drill-down pattern.
- **Interpretable.** Entity sub-communities have clear names (the most-frequent entity is the cluster's label).
- **Reuses S2.** The chunk-level Leiden is the same as S2; only the entity layer is added.

**Cons:**
- **Two Leiden runs.** Chunk-level + entity-level. ~30s + ~10s for 60k corpus. Recompute cadence is the same.
- **Two LLM summarization passes.** The entity-level summaries feed the chunk-level summaries. More LLM calls, more cost.
- **Complexity.** The data model has chunk communities, entity communities, and a hierarchy between them. Operationally heavier.

**Verdict:** the long-term target. S2 is the v1; S5 is the v2 once S2 has been validated on LoCoMo.

---

## Possible solutions — three implementation paths

### Path A — Lightweight Leiden on the chunk graph (S2)

**Build:** the natural v1.

**Pieces:**
1. `internal/memory/communities.go` — Leiden partition of the chunk graph. Power iteration, modularity optimization. Same library code as PageRank.
2. `db/migrations/011_chunk_communities.sql` — new table `(chunk_id TEXT, community_id INT, level INT, computed_at TIMESTAMPTZ)`. PK `(chunk_id, level)`.
3. `db/migrations/012_community_summaries.sql` — new table `(community_id INT, level INT, summary TEXT, embedding VECTOR(768), computed_at TIMESTAMPTZ)`. PK `(community_id, level)`.
4. `internal/llm/synthesis.go` — the community-summary LLM prompt template. Called by the community compute job.
5. `cmd/community-recompute/main.go` — background job. Triggered by the same ticker as PageRank (every 6h) or on 5% delta.
6. `internal/memory/query.go` — extend with a `QueryGlobal(question, top_communities)` path. Embeds the question, finds the top-K communities by embedding similarity to the community summary, returns the summaries + the top chunks from each community.

**Query-time flow:**
```
QueryGlobal("What are the main themes about family?")
  1. Embed the question
  2. Find top-5 community summaries by embedding similarity
  3. For each community, fetch the top-3 chunks (by PageRank within the community)
  4. Pass the 5 summaries + 15 chunks to MemoryAnswerAgent (ADR-0055)
  5. Return the synthesized answer with citations
```

**Pros:**
- Cheap, reuses existing primitives, fits Cambrian's chunk-anchored model.
- The community summary IS a derived signal (like PageRank) — pre-computed, cached, fail-soft.

**Cons:**
- Chunk-level clusters are less interpretable than entity-level.
- The summarization prompt has to bridge from chunks to themes.

**Estimate:** 1 week. Same recompute cadence as PageRank (6h). LoCoMo validation: 2 days.

---

### Path B — Full GraphRAG (S1)

**Build:** the canonical Microsoft pattern.

**Pieces:**
1. `agents/entity_extractor_agent.py` — system agent, like kg_extractor. LLM extracts entities (name, kind, description) from each chunk.
2. `internal/entities/store.go` — the `entities` and `entity_relations` tables.
3. `internal/entities/graph.go` — Leiden over the entity graph. Hierarchical (multiple resolutions).
4. `db/migrations/013_entities.sql` — `entities (id, name, kind, description, embedding, chunk_count)`, `entity_relations (h, r, t, weight, confidence)`, `entity_communities (entity_id, community_id, level)`.
5. `cmd/community-recompute/main.go` — extended. Recomputes entities + entity communities + entity summaries.
6. `internal/memory/query.go` — extend with `QueryGlobal` that drills into entity communities.

**Pros:**
- The canonical reference. Most interpretable clusters. Hierarchical drill-down.

**Cons:**
- **Entity extraction is an LLM call per chunk.** Expensive. Drift from `chunk_triplets`.
- **Two parallel KG layers.** Operationally heavy.
- **Slow to validate.** The first corpus compute is 60k LLM calls = hours of LLM time.

**Estimate:** 3-4 weeks. LoCoMo validation: 1 week.

---

### Path C — Hybrid: Path A + entity-level drill-down (S5)

**Build:** Path A first; layer the entity layer on top after Path A is validated.

**Pieces (incremental on Path A):**
1. **Phase 1 — Path A** (1 week). Chunk-level Leiden, community summaries, global query path.
2. **Phase 2 — entity layer** (2 weeks). `entity_extractor_agent.py` extracts entities for the **top-K communities only** (not every chunk). Entity-level Leiden within each chunk community. Entity-community summaries feed the chunk-community summaries as additional context.
3. **Phase 3 — hierarchical query** (1 week). `QueryGlobal` supports drill-down: "tell me more about theme X" maps to the sub-community.

**Pros:**
- **Incremental.** Path A ships first. The entity layer is added only where it adds value (top-K communities).
- **Avoids the full-graph cost.** We don't extract entities for the 95% of chunks that aren't in the top communities.
- **Hierarchical.** The user gets both chunk-level themes and entity-level drill-down.

**Cons:**
- **Two Leiden runs.** Chunk + entity. Same recompute cadence.
- **Two summarization passes.** Chunk summaries + entity summaries. More LLM cost.
- **Complexity.** Two layers of clustering, two summarization jobs.

**Estimate:** Path A = 1 week. Path C total = 4-5 weeks. LoCoMo validation: 2 days per phase.

---

## Trade-off matrix

| axis | Path A | Path B | Path C |
|---|---|---|---|
| build effort | 1 week | 3-4 weeks | 4-5 weeks |
| LLM cost per recompute | ~$0.10 (100 community summaries) | ~$60 (60k entity extractions) | ~$0.50 (Path A + top-K entity layer) |
| LoCoMo validation | 2 days | 1 week | 2 days per phase |
| interpretability | medium (chunk-level) | high (entity-level) | high (both) |
| drill-down | no | yes (hierarchical Leiden) | yes |
| fits Cambrian's chunk model | yes | no (parallel entity graph) | yes |
| operational complexity | low | high | medium |
| fail-soft if no clusters | yes (return "no themes") | no (must have entities) | yes (Path A's behavior) |

---

## Recommendation

**Path A first, Path C as a v2 if Path A proves insufficient.**

Reasoning:
- Path A is the smallest change that delivers Layer 3. It's a 1-week build that fits Cambrian's chunk-anchored model and reuses the existing primitives (chunk graph from `chunk_triplets`, MemoryAnswerAgent for synthesis, the recompute ticker from PageRank).
- Path B is the canonical Microsoft GraphRAG but the cost (60k LLM calls per recompute) is wrong for Cambrian's small-corpus, low-latency deployment profile.
- Path C is the long-term target but the entity layer is expensive to validate. Wait until Path A has been validated on LoCoMo and the user has a feel for what "community" means at the chunk level.

**The validation criteria** for Path A:
- The community summaries are interpretable (a human can read the summary and understand the theme).
- The `QueryGlobal` path beats the LLM-only summarization (S4) on at least 3 of 5 LoCoMo global queries.
- The recompute is fast (< 5 minutes for a 60k-chunk corpus) and cheap (< $0.20 per recompute).
- The fail-soft behavior works: a corpus with no communities (cold start) returns "insufficient data" without crashing.

**The migration path:**
- Path A ships as ADR-0057 (proposed after this discussion).
- After 1-2 months of Path A in production, the user decides whether the entity layer (Path C) is worth the cost.
- Path B is rejected outright; the cost is wrong for Cambrian.

---

## Open questions

1. **Hierarchical Leiden at multiple resolutions.** Single resolution (default) or hierarchical (3 levels)? LoCoMo's 10 conversations fit in one level; a 600k-chunk production corpus might need 3.
2. **Community summary prompt.** The prompt has to bridge from chunks to themes. What's the v1 prompt? (Will be designed as part of Path A implementation.)
3. **Cold-start.** A new corpus with 100 chunks has noisy clusters. How many chunks is "enough"? (Threshold: ≥ 500 chunks for Leiden to produce stable communities.)
4. **Time-windowed communities.** A "themes in 2024" query should not include 2023 communities. Should communities be time-windowed, or query-time filtered? (Query-time filter is simpler.)
5. **Cross-corpus communities.** A user with multiple corpora (work + personal) might want communities across both. Path A is per-corpus; cross-corpus is a v2.
6. **Entity extraction for the top-K communities only** (Path C Phase 2) — what K? (Will be tuned on LoCoMo.)

---

## Literature anchor

| Claim | Source |
|---|---|
| **Community detection + LLM summaries for global queries** | Microsoft GraphRAG (Edge et al., 2024, arXiv:2404.16130) — the canonical reference |
| **Leiden algorithm for community detection** | Traag et al., 2019, Scientific Reports 9 — faster and higher-quality than Louvain |
| **Embedding-based topic modeling** | BERTopic (Grootendorst, 2022) + Top2Vec (Angelov, 2020, arXiv:2008.09470) — the S3 alternative |
| **Hierarchical community summaries** | Microsoft GraphRAG §3.3 — the drill-down pattern |
| **KG-anchored communities** | KG²RAG §3.2 — chunks as the unit, entities as the bridge |
| **Derived signals in Cambrian** | ADR-0054 (PageRank as a pre-computed, cached signal) — the architectural pattern |

---

## Appendix A — The community-summary prompt (v1, draft)

```
SYSTEM
You are CommunitySummarizer, a Cambrian system component. You summarize a
cluster of related chunks into a 1-3 sentence theme description.

INPUTS
- A community_id (integer)
- A list of chunks (chunk_id, text, entities[] from chunk_triplets)
- The community's top-10 most-frequent entities (computed from the members)

OUTPUT
JSON:
{
  "community_id": <int>,
  "summary": "<1-3 sentence theme description>",
  "key_entities": ["<entity>", ...],
  "representative_chunk_ids": ["<chunk_id>", ...]  // 3-5 chunks that best exemplify the theme
}

RULES
- The summary is about the community as a whole, not any single chunk.
- The key_entities are the most informative entities for retrieval.
- The representative_chunk_ids are the chunks a retrieval should return if
  the question matches this community.
- If the community is too noisy to summarize, return {"summary": "unclear",
  "key_entities": [], "representative_chunk_ids": []}.
```

## Appendix B — What stays the same

- **D1-D4 from ADR-0053** — unchanged. Layer 3 is additive on top of the chunk-level KG²RAG pipeline.
- **D2 from ADR-0053 (revised)** — the kg_extractor's per-chunk `(h, r, t)` triplets are the input to the chunk graph. The communities are a derived view, not a parallel KG.
- **ADR-0054 (multi-signal ranking)** — the PageRank scores from the multi-signal blend are a sibling by-product of the Leiden partition. They share the recompute ticker.
- **ADR-0055 (MemoryAnswerAgent)** — unchanged. The community summary is fed to the MemoryAnswerAgent as the context for a global query. The agent is the synthesis layer for both local and global queries.
- **The Cambrian Hex** — preserved. Layer 3 is a derived signal (deterministic reflex). The query routing is the Awareness layer.

## Appendix C — Decision (TBD)

This is a Discussion ADR. The user picks Path A, B, or C. The chosen path becomes a Proposed ADR (ADR-0057) with the implementation details fleshed out.
