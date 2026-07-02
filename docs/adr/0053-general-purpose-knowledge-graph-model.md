# ADR-0053: Chunks as the Memory Unit — KG²RAG-aligned Retrieval for Cambrian

**Status:** Proposed (2026-06-24, rewritten 2026-06-24; **D2 revised 2026-06-25** — extraction moves from a single write-time LLM call to the frozen `kg_extractors/` tiered pipeline; **D3 partially implemented 2026-06-25** — only vector seed + one-hop `kgExpand` are built; the graph-filter/rerank/synthesis stages are superseded by ADR-0054 + ADR-0055, see the Decision 3 status note) — supersedes the prior 4+7 model and the Wikidata 2+2 model. The foundation is **chunks** (text + embedding + id) with **per-chunk triplets** (h, r, t). The retrieval pipeline is the KG²RAG pattern: vector seed → KG expansion → graph filter → reranker. The kernel-side `MemoryProvider` (D10) is preserved. The prior 4-type vocabulary, bi-temporal model, and community detection are **deferred to layered enhancements** (Layer 1-5 below), not schema-level decisions.
**Date:** 2026-06-24 (D2 revised 2026-06-25)
**Author:** Afsin
**Depends on:** ADR-0015 (Engram engine — vector search), ADR-0017 (spreading activation — KG expansion), ADR-0025 (memory reform), ADR-0034 (tag-based scope), ADR-0042 (centralized LLM provider — extraction + reranker), ADR-0049 (Experiential Memory — event-sourcing pattern, optional).
**Foundational citation:** KG²RAG (Zhu et al., 2025, arXiv:2502.06864, NAACL 2025) — the chunks-as-memory model + KG-guided chunk expansion + paragraph organization. Code: https://github.com/nju-websoft/KG2RAG

---

## Context

ADR-0049 built a **typed** memory substrate (`mnemonic_fact` / `mnemonic_scene` / `mnemonic_action` / `mnemonic_entity`) for an LLM-agent use case. The prior ADR-0053 v1 proposed a 4-type / 7-predicate knowledge graph model. The v2 proposed a Wikidata-style 2+2 model. **Both were over-engineered.**

The user pointed to the KG²RAG paper (arXiv:2502.06864) and its implementation. The actual implementation is **a small, pragmatic system** (~200 lines of Python using llama_index + NetworkX + bge-reranker). It solves a specific problem: multi-hop QA over Wikipedia paragraphs. The chunks model is the right primitive because **chunks are what you have** — you don't extract entities at runtime; you pre-extract triplets once and use them for retrieval.

**The lesson:** the foundational primitive is the **chunk** (text + embedding + id), with **per-chunk triplets** (h, r, t) as the KG. The 4-type vocabulary, bi-temporal model, and community detection are **layered enhancements** on top, not schema-level decisions. The storage is the chunks; the retrieval is KG²RAG; the rest is buildable on top.

This ADR captures only the **foundation** — the chunks model + KG²RAG retrieval + kernel-side provider. The 5 layered enhancements (bi-temporal, event-sourced state, community detection, recency-aware ranking, aggregation) are documented as future work, not decisions.

---

## Decision 1 — Chunks as the irreducible memory unit

A chunk is a piece of text (typically 200-500 tokens) with a vector embedding and a stable ID. The chunk carries its source as metadata; it does NOT carry a type (no `mnemonic_fact` / `mnemonic_scene` / `mnemonic_action` enum) because the type is emergent from the chunk's text and metadata, not from a fixed schema.

```sql
chunks (
  id          TEXT PRIMARY KEY,         -- stable IRI; default is '{doc_id}##{chunk_index}'
  text        TEXT,                     -- the chunk text
  embedding   VECTOR(768),              -- the embedding
  source_id   TEXT,                     -- the source doc/event that produced the chunk
  metadata    JSONB,                    -- flexible: {author, created_at, type_hint, ...}
  created_at  TIMESTAMPTZ
);
```

**The schema is one table.** No `document_type` ENUM. No separate `mnemonic_entity` table. No sub-types. Every piece of memory is a chunk.

**Why chunks converge across domains:** every domain's "thing" is a chunk of text. A person (Caroline) is a chunk whose text is "Caroline". A fact is a chunk whose text is "Caroline researched adoption". A measurement is a chunk whose text is "175.3 (temperature, 2026-04-21T10:23:45Z)". A clause is a chunk whose text is "Section 4.2: Termination upon 30 days notice". **No per-domain schema work.** The Cambrian existing `documents` table IS the chunks table; this ADR just renames it conceptually and drops the `document_type` column's strict enum.

**Why this is the user's "entity is a sum of facts" intuition formalized:** the entity is the SET of chunks mentioning it, computed at query time. No stored entity text to drift. "Caroline is dead" is a NEW chunk; it doesn't update any prior chunk. The query "is Caroline alive?" is "find the most recent chunk mentioning Caroline's status with `valid_to IS NULL`". The set is computed; the chunks are the source of truth.

---

## Decision 2 — Per-chunk triplets via a tiered, LLM-free-first extractor (revised 2026-06-25)

> **Revision note (2026-06-25).** D2 originally specified that triplets were extracted "at write time, by the LLM," batched via the `EdgeBatcher`. The LoCoMo sweep in `notebooks/kg_extractors_comparison.ipynb` (writeup: `kg_extractors/SUMMARY.md`; ADR driver: `kg_extractors/WHAT_TO_DO.md`) showed a free, deterministic, CPU-only extractor matches the LLM on the retrieval-relevant metric at zero LLM cost. **The KG primitive (per-chunk `(h, r, t)` triplets) and the KG²RAG retrieval path (D3) are unchanged.** Only the *producer* of the triplets changes: from one LLM call per chunk to a tiered pipeline with the LLM demoted to an opt-in residue. This makes per-chunk extraction part of the deterministic **Reflexive Path (Omurilik)**, not the Awareness layer — the Zero-Hardcode Rule is preserved (it governs agent↔task routing, not the reflex extraction; see CLAUDE.md).

The KG is **per-chunk**: each chunk has a list of `(h, r, t)` triplets extracted from its text, stored alongside the chunk. The triplets are produced at write time by a **three-tier extractor**, and every triplet carries **how confident we are and who produced it**:

```sql
chunk_triplets (
  chunk_id    TEXT NOT NULL,           -- → chunks.id
  h           TEXT NOT NULL,           -- head entity (free-form string; canonicalize on insert)
  r           TEXT NOT NULL,           -- relation (free-form verb phrase)
  t           TEXT NOT NULL,           -- tail entity (free-form string)
  weight      REAL NOT NULL DEFAULT 1.0,  -- extractor's own per-triple weight
  confidence  SMALLINT,                -- agreement tier: 2=high, 1=low, 0=filler, NULL=legacy (added migration 009)
  sources     TEXT[],                  -- producers: any of {metadata, spacy_patterns, llm} (added migration 009)
  PRIMARY KEY (chunk_id, h, r, t)
);
```

### The tiered pipeline (frozen in `kg_extractors/`)

For every chunk, the Substrate ingester runs (in order, all sharing the chunk's `(text, dia_id, speaker, date)`):

| tier | producer | what it extracts | cost | role |
|------|----------|------------------|------|------|
| 1 | `metadata` | structural `[date] Speaker:` header → `spoke to` / `dated at` / `spoke at` | ~0.01 ms/chunk | **always-on** backbone (strictly complementary, pair-Jaccard ≤ 0.001 with every other tier) |
| 2 | `spacy_patterns` | dep-parse SVO + Hearst / appositive / possessive / acl / relcl over the body | ~10 ms/chunk (CPU) | **always-on** workhorse |
| 3 | `llm` residue | the existing `<h##r##t>` LLM prompt, run **only** where confidence is low | 1 LLM call/chunk | **opt-in**, gated; fills singletons the rule stack misses |

The producers are union-ed, then deduplicated on `(chunk_id, h, r, t)`. Each surviving triplet is labelled:

- **`confidence = 2` (high)** — ≥ 2 tiers independently produced it (after entity-soft-match), OR a single high-precision deterministic pattern.
- **`confidence = 1` (low)** — a single tier produced it.
- **`confidence = 0` (filler)** — produced only by the LLM residue tier.
- **`confidence = NULL` (legacy)** — pre-migration rows (all from the old LLM batcher); treated as filler by readers.

`sources[]` records which tiers produced the triplet (e.g. `{metadata}`, `{spacy_patterns,llm}`), so the labelling is auditable and a future ensemble/training run can attribute each triplet.

### Why LLM-free-first (evidence)

From `kg_extractors/SUMMARY.md` (LoCoMo, 5,880 canonical chunks, 10 conversations):

- `spacy_patterns` reaches **graph_recall 0.994** vs the LLM reference's **0.997** — within noise on QA-evidence reach — at **0 LLM cost** and ~96 chunks/s on one CPU thread.
- pair-Jaccard between the rule stack and the LLM is **≤ 0.05**: the LLM is *complementary, not redundant* — it extracts ~5% of pairs the rule stack cannot see. That is why the LLM **stays** as a residue/agreement oracle rather than being removed.
- `graph_recall` is a connectivity proxy that rewards volume; the per-triple quality signal (`entity_f1_soft` ≈ 0.71 for `spacy_patterns` vs an *unverified* LLM reference) is weaker and is exactly why `confidence`/`sources[]` exist: the routing layer (D3 / WHAT_TO_DO §1) trusts high-confidence triplets directly and sends low-confidence ones to the LLM residue.

**This is a different shape from the prior `document_edges` table.** The prior table connected documents to documents (doc → doc edges with typed predicates). `chunk_triplets` is **per-chunk** — each row is a triplet observed in that chunk's text. The `h`/`t` are entity strings (canonicalized to lowercase), not doc IDs.

**The retrieval path is chunk-anchored and producer-agnostic:** a query "What did Caroline research?" → vector search for seed chunks → for each seed chunk, get its triplets from `chunk_triplets` → the triplets point to other entities → pull in chunks that mention them → re-rank. **The KG is a navigation graph between chunks**; D3 reads `chunk_triplets` identically regardless of which tier produced a row (it may additionally filter by `confidence` once routing is wired).

### The LLM residue prompt (tier 3, unchanged from the original D2)

```
Extract informative triplets from the text following the examples.
Make sure the triplet texts are only directly from the given text.

Examples:
  Text: Scott Derrickson (born July 16, 1966) is an American director.
  Triplets: <Scott Derrickson##born in##1966>$$<Scott Derrickson##nationality##America>$$<Scott Derrickson##occupation##director>

  Text: Caroline researched adoption agencies for her family.
  Triplets: <Caroline##researched##adoption agencies>$$<Caroline##has family##family>

Text: {chunk_text}
Triplets:
```

The LLM outputs `<h##r##t>` triplets separated by `$$`; the kernel parses, lowercases/trims entities, and inserts with `sources={llm}`. This matches the KG²RAG paper (`preprocess/hotpot_extraction.py`). It now runs **only** on chunks the rule stack left under-covered, not on every chunk.

---

## Decision 3 — The KG²RAG retrieval pipeline

> **Implementation status (2026-06-25).** Of the five stages below, only **vector seed → KG expansion (one-hop)** is implemented and wired (`internal/memory/kg_expand.go`, invoked from `QueryService.recall` at `query.go:313`). The **graph filter** (connected-components) and the **reranker/organize** stages were *never built* — expanded chunks are scored by `expandedScore` (a 0.5 survival floor, lifted by query→chunk cosine; the seam the blend extends), not by a connected-component filter or a cross-encoder. The pseudocode in 3.1–3.3 below is the *design sketch*, not the shipped code. **The ranking layer D3 gestured at is delivered by ADR-0054** (multi-signal blend + PageRank + bge-reranker), which supersedes the `graphFilter`/`rerank` sketch; **the synthesis (`Ask` / `organize`) is delivered by ADR-0055** (MemoryAnswerAgent). Treat 3.1 (`kgExpand`) as authoritative and 3.2/3.3 as superseded design notes.

The retrieval pipeline is the **KG²RAG pattern**: vector seed → KG expansion → graph filter → reranker → answer.

```go
// internal/memory/kg_rag.go

func KG2RAGRetrieve(ctx context.Context, q RecallQuery) (*AskResult, error) {
    // 1. Vector search: seed chunks
    seedChunks := vectorStore.Search(ctx, q.Embedding, topK=20)

    // 2. KG expansion: for each seed chunk, get its triplets, then get
    //    the chunks that share entities. One hop.
    expanded := kgExpand(ctx, seedChunks, hops=1)

    // 3. Graph filter: build a chunk graph, find connected components,
    //    pick the top-k by score. (NetworkX equivalent in Go.)
    candidates := graphFilter(expanded, topK=10)

    // 4. Rerank: if a reranker is available, use it. Otherwise, use cosine.
    ranked := rerank(ctx, q, candidates)

    // 5. Organize into a paragraph (post-process, optional)
    return organize(ranked)
}
```

**Each step is a Go function that the kernel exposes.** The agent SDK calls the high-level `MemoryProvider.Ask(question)` which runs the full pipeline. The kernel-side ownership means:
- All four steps are testable in isolation
- The pipeline is observable (which step contributed what)
- The router (D10) can pick between this pipeline and lower-level tools

### 3.1 — KG expansion (the key step)

```go
// kgExpand returns the union of seed chunks and their KG-expanded neighbors.
func kgExpand(ctx context.Context, seeds []Chunk) []Chunk {
    seen := make(map[string]bool)
    for _, c := range seeds { seen[c.ID] = true }
    out := append([]Chunk{}, seeds...)

    // For each seed chunk, get its triplets
    triplets := chunkTriplets.GetForChunks(ctx, seedIDs)

    // Build a set of entities mentioned in the seed triplets
    entities := make(map[string]bool)
    for _, t := range triplets {
        entities[normalize(t.H)] = true
        entities[normalize(t.T)] = true
    }

    // For each entity, get all chunks that mention it (vector search on entity name)
    for e := range entities {
        // Lightweight lookup: chunks whose text contains the entity name
        // OR an embedding lookup for entity-style queries
        related := chunksContainingEntity(ctx, e, topK=5)
        for _, c := range related {
            if !seen[c.ID] {
                seen[c.ID] = true
                out = append(out, c)
            }
        }
    }
    return out
}
```

This is the **chunks-anchored KG expansion**. The KG is a navigation graph; the seed chunks are the entry points; the expansion pulls in related chunks via shared entities.

### 3.2 — Graph filter (the "global" reasoning step)

```go
// graphFilter builds a chunk graph and picks the top-k connected components.
func graphFilter(chunks []Chunk, topK int) []Chunk {
    g := NewChunkGraph()
    for _, c := range chunks {
        triplets := chunkTriplets.GetForChunk(c.ID)
        for _, t := range triplets {
            // Find other chunks that contain the entity t.H or t.T
            other := chunksContainingEntity(t.H)
            for _, o := range other {
                g.AddEdge(c.ID, o.ID, weight=c.Score+o.Score, rel=t.R)
            }
            // ... same for t.T
        }
    }
    // Find connected components, score by sum of edge weights, pick top-k
    return g.TopComponents(topK)
}
```

This is the **"global" trigger** (per T-Mem and GraphRAG). The graph filter finds coherent clusters of related chunks, not just top-k vector hits.

### 3.3 — The re-ranker

If a re-ranker (bge-reranker-large or similar) is available, use it on the top-k candidates. Otherwise, fall back to cosine score. The re-ranker is a LATER enhancement (Phase 3); the cosine fallback works for the v1 proof.

---

## Decision 4 — Kernel-side MemoryProvider, thin SDK wrapper

Unchanged from the prior ADR-0053 v1 (D10). The kernel owns the entire memory stack end-to-end:

- The `MemoryProvider` struct in `internal/memory/provider.go`
- The gRPC service `MemoryProvider` in `proto/cambrian.proto`
- The `Ask` high-level entry point + the low-level `RecallFacts` / `RecallEntities` / `RecallTopics` / `RecallByID` / `RecallPath` / `RecallAtTime` RPCs
- The SDK is ~30 lines of gRPC + dataclass marshalling per language
- Tests are in-process and run in microseconds

**The D10 cut is what makes this ADR testable.** The kernel-side `KG2RAGRetrieve` function is testable with fakes (`embedder`, `vectorStore`, `chunkTripletsStore`); the gRPC server is not needed. The SDK is a thin wrapper.

The existing gRPC methods (`QueryMemory`, `IngestMemory`) are deprecated forwarders. The new gRPC service uses the same proto IDL.

---

## Decision 5 — The 5 layered enhancements (DEFERRED, not foundational)

The KG²RAG pattern delivers the **50% that HotpotQA proves works**: multi-hop discovery, document retrieval, relation traversal. The other 50% is what makes a "company brain" feel alive. The 5 enhancements are **layers** built on top of the chunks foundation, not schema-level decisions. Each is a small, additive change.

### Layer 1 — Bi-temporal chunks
Add `valid_from` / `valid_to` columns to `chunks`. The LLM extraction prompt asks "when did this become true in the world?" The query "what was true in March 2024?" is a SQL filter. **~1 week. T-Mem-style temporal QA.**

### Layer 2 — Event-sourced state
A `type='mnemonic_event'` chunk for state changes. "Caroline changed jobs on 2024-03-15" is a chunk with `type='event'`. The "current state" of Caroline is the most recent event-chunk about her, filtered by `valid_to IS NULL`. **~1 week. ADR-0049 D9 already implements the field-LWW; this is the chunk-level equivalent.**

### Layer 3 — Community detection
Leiden on the chunk graph. Community summaries as `mnemonic_topic` chunks (e.g., "Q3 finance", "engineering", "hiring"). The query "what are the main themes?" is a vector search over topic chunks. **~2 weeks. Microsoft GraphRAG pattern.**

### Layer 4 — Recency-aware ranking
Blend `cosine` × `α` + `recency` × `β` + `temporal_decay` × `γ`. The query "what did we decide last week?" re-ranks with recency upweighted. **~3 days. Cambrian already has `temporal_decay.go` (ADR-0015); just expose it as a knob.**

### Layer 5 — Aggregation
A "summarize N chunks" function that uses the LLM to compress. The summary is itself a `mnemonic_topic` chunk (cached for future queries). **~1 week. Microsoft GraphRAG's map-reduce pattern.**

**Each layer is independent.** The foundation (KG²RAG) works without any of them. The layers compose: a query for "what did we decide last week about X" uses the foundation (find X chunks) + Layer 1 (filter by time) + Layer 4 (re-rank by recency) + Layer 5 (summarize).

---

## What this preserves from the prior ADR

- **D10 (kernel-side MemoryProvider)**: preserved unchanged. The architectural cut is the same.
- **The gRPC surface**: preserved. The new gRPC service uses the same proto IDL.
- **The testability win**: preserved. In-process tests with fakes.
- **The "domain adaptability" claim**: preserved (and strengthened). The chunks model converges to every domain without per-domain schema work.
- **The "entity is a sum of facts" intuition**: preserved. The entity is the set of chunks; no stored entity text.

## What this changes from the prior ADR

- **D1 (four node types)**: REMOVED. The 4-type vocabulary is a future enhancement (the `type_hint` metadata on a chunk), not a schema-level decision.
- **D2 (seven predicates)**: REMOVED. The predicates are free-form verb phrases in the LLM extraction prompt. The schema doesn't constrain them.
- **D3 (event-sourced state)**: DEFERRED to Layer 2.
- **D4 (bi-temporal model)**: DEFERRED to Layer 1.
- **D5 (community detection)**: DEFERRED to Layer 3.
- **D6 (one edge table polymorphic)**: REPLACED with `chunk_triplets` (per-chunk h, r, t). The shape is different — per-chunk not global.
- **D7 (universal IRI)**: preserved; chunks have IRI-style IDs (`{doc}##{chunk_index}`).
- **D8 (LLM extraction)**: SIMPLIFIED to per-chunk triplets in `<h##r##t>` format. No JSON, no entity types, no predicates vocabulary. **Revised 2026-06-25:** the LLM is no longer the *primary* producer — extraction is the tiered `metadata` + `spacy_patterns` + LLM-residue pipeline (revised D2), with per-triple `(confidence, sources[])`.
- **D9 (domain adaptability)**: preserved; zero per-domain schema work.
- **D11 (chunks-as-memory)**: simplified to the actual KG²RAG minimum (chunks + chunk_triplets, no qualifiers, no separate edges, no community detection).

## What was wrong with the prior ADR

- **Over-engineered the 4-type / 7-predicate model.** The Cambrian existing `document_type` ENUM (`mnemonic_fact` / etc.) was a vocabulary, not a schema. I made it a schema.
- **Tried to converge to "all knowledge" with the Wikidata 2+2 model.** The storage cost (5× current) and query complexity (2-3 joins) were real. The chunks model converges without the cost.
- **The "entity-text drift" problem** was solved by deriving the entity from the chunks (not by storing it). The user's intuition was right; my prior model added a stored entity layer on top of the chunks. That's redundant.
- **The "4-type vocabulary" was a domain guess, not a foundational schema.** The Cambrian existing types are emergent from the chunks' text and metadata, not from a fixed enum.

---

## The KG²RAG implementation in the kernel

### Phase 0 — Minimal viable KG²RAG (this ADR's scope)

The minimum implementation that delivers the HotpotQA improvement:

1. **Schema** (one migration):
   - Rename `documents` → `chunks` (no schema change; conceptual rename)
   - Rename `document_edges` → keep as `chunk_kg` (semantic rename; the schema stays compatible with the new per-chunk triplets model — `source_id` becomes `chunk_id`, `target_id` becomes the entity name, `relation` becomes the relation string)
   - Add `chunks.metadata` JSONB (flexible metadata, including `type_hint` for Layer 1+)
   - Add `chunks.source_id` column (provenance, even if not used by KG²RAG)

2. **Triplet extraction** (per-chunk, tiered — revised 2026-06-25; **wired 2026-06-25**):
   - Tier 1 `metadata` + Tier 2 `spacy_patterns` run inside the **`kg_extractor` system agent** (`agents/kg_extractor_agent.py`, a NO-LLM `DeterministicAgent`), frozen in `kg_extractors/`. Tier 3 `llm` residue stays the original path.
   - **Kernel-side wiring (decided):** the rule tiers run as a privileged **system organ** — the same pattern as the pre-plan Scout (ADR-0051). `kg_extractor_agent` is registered in `domain.IsSystemAgent`, so it bypasses the auction, the Gatekeeper candidate pool, and the interview; the kernel hands it a chunk batch as a `Handoff` and gets triplets back, invoked DIRECTLY via `Auctioneer.CallAgent` (no auction). The Go side is a thin adapter `KgExtractorDispatcher` (`internal/substrate/network/kg_extractor_dispatch.go`) implementing the `memory.TripletExtractor` port.
   - **Ingest hot path:** the existing `ChunkTripletsBatcher` keeps its async queue/drain/persistence machinery (decoupling ingest from extraction); only the *producer* is swapped — `main.go` injects the dispatcher via `ChunkTripletsBatcher.UseExtractor` when `execution.kg_extractor_enabled` is set. Off (default) ⇒ the legacy LLM extractor. The LLM-batching motive is gone (the organ is ~10 ms/chunk), so the batch is now just gRPC-round-trip amortization for bulk ingest, not latency hiding.
   - Output: rows in `chunk_triplets` with `(weight, confidence, sources[])` — the agent stamps `sources` (which tiers fired) and `confidence` (2 if ≥2 tiers agree, else 1); the LLM adapter stamps `sources={llm}, confidence=0` (filler).
   - The original single-LLM `chunk_extractor.go` / `chunk_triplets_batcher.go` LLM path remains valid as the Tier-3 residue producer (the default when the flag is off).

3. **KG expansion** (the retrieval post-processor):
   - New `internal/memory/kg_retrieval.go` (Go)
   - `kgExpand(ctx, seedChunks, hops=1)` — for each seed chunk, get its triplets from `chunk_kg`, get other chunks that share entities, return union
   - Wired into the `MemoryProvider.Ask` pipeline as a post-processor after vector search

4. **MemoryProvider updates** (D10, preserved):
   - `MemoryProvider.Ask` now runs: `vector_seed → kg_expand → naive_rerank → answer`
   - The gRPC surface is unchanged
   - Tests are in-process

5. **Tests** (fast, in-process):
   - `TestKGExpand_SingleHop`: seed chunk mentions "Caroline" + "adoption"; expansion should include the chunk that mentions "Caroline" + "PhD"
   - `TestKGExpand_NoTriplets`: seed chunk with no triplets should return just the seed
   - `TestAsk_PicksKGExpandedResults`: full pipeline, vector+KG should beat vector-only on a multi-hop question
   - The existing LoCoMo benchmark is the integration test

### Phase 1+ — The 5 layers (future ADRs)

Each is a small, additive change:

- **Phase 1** (Layer 1: bi-temporal): add `valid_from` / `valid_to` columns, update LLM prompt, update query path. **~1 week.**
- **Phase 2** (Layer 2: event-sourced): add `type='event'` chunks, fold the state. **~1 week.**
- **Phase 3** (Layer 3: community detection): Leiden on the chunk graph, topic chunks. **~2 weeks.**
- **Phase 4** (Layer 4: recency-aware ranking): blend cosine + recency + decay. **~3 days.**
- **Phase 5** (Layer 5: aggregation): LLM-summarize N chunks. **~1 week.**

---

## Literature anchor

| Claim | Source |
|---|---|
| **Chunks as the irreducible memory unit; per-chunk triplets as the KG; vector seed → KG expansion → graph filter → rerank pipeline** | **KG²RAG (Zhu et al., 2025, arXiv:2502.06864, NAACL 2025)** — the foundational citation. Code: https://github.com/nju-websoft/KG2RAG |
| Vector seed → KG expansion → paragraph organization | KG²RAG paper §3.2 |
| Per-chunk triplet extraction in `<h##r##t>` format | KG²RAG `preprocess/hotpot_extraction.py` (their prompt + parser) |
| Graph-based filtering using connected components + MST | KG²RAG `util/kg_post_processor.py::GraphFilterPostProcessor` |
| Vector + KG hybrid retrieval (descriptive + associative) | T-Mem (arXiv:2606.15405), Microsoft GraphRAG (arXiv:2404.16130) |
| Re-ranking with cross-encoder | bge-reranker-large (KG²RAG's choice); production systems (Cohere Rerank, etc.) |
| Kernel-side orchestration pattern (orchestrator owns retrieval) | Cambrian's own LLM provider (ADR-0042), tool registry (ADR-0039), ContentStore (ADR-0048) — same cut, applied to memory |
| Community detection for global queries (Layer 3) | Microsoft GraphRAG (arXiv:2404.16130) — Leiden algorithm |
| Bi-temporal model (Layer 1) | T-Mem §3.3, Episodic-Semantic (arXiv:2605.17625) |
| Event-sourced state (Layer 2) | Orchestrated Reality (arXiv:2606.16014), DCPM (arXiv:2606.09483) |
| Universal noun convergence (chunks work for every domain) | Wikidata (the same principle at higher storage cost), Cambrian ADR-0049 |

---

## Open questions

1. **What is the chunking strategy?** D2 assumes chunks are 200-500 tokens, but the chunking decision matters. Too small = loss of context, too many edges. Too large = irrelevant text dilutes the embedding. Options: (a) fixed-token chunks (e.g., 256 tokens), (b) semantic chunks (split at paragraph / section boundaries), (c) LLM-chunked. D2 is silent on this; a follow-up ADR for the chunking pipeline.
2. **What is the right balance for KG expansion depth?** Vector search returns seed chunks; KG expansion follows chunk-to-chunk edges. Expansion depth=1 (direct neighbors) vs depth=2 (one hop further) trades recall for noise. The empirical answer needs the LoCoMo benchmark. The router's `max_depth` config is the dial.
3. **What is the re-ranker?** The bge-reranker-large is large and local. Production alternatives: Cohere Rerank (API), cross-encoder/ms-marco-MiniLM (smaller), LLM-as-reranker (use the LLM to score chunks). The right answer depends on the cost/latency budget.
4. **What is the entity canonicalization policy?** The LLM extracts "Caroline" / "caroline" / "Ms. X" / "she" — all refer to the same entity. The current KG²RAG implementation uses 3-gram n-gram overlap (heuristic). A better approach: embedding similarity on entity strings, or a learned coreference model. D2 uses simple lowercase normalization; a follow-up can improve.
5. **What is the LLM extraction prompt at write time vs pre-process time?** KG²RAG pre-extracts triplets offline (one-time cost per dataset). Cambrian extracts at write time (per-ingest cost). The trade-off: pre-extract is faster at query but stale; write-time is fresh but slower. D2 picks write-time (per-chunk extraction in the batcher). A follow-up can add a pre-extract mode for static corpora.
6. **What about multi-turn / cross-session memory?** The chunks model handles per-conversation data well (each LoCoMo turn is a chunk). For cross-session aggregation (e.g., "what have we learned about Caroline across all our conversations?"), Layer 3 (community detection) and Layer 5 (aggregation) are the answers.

---

## Appendix A — The KG²RAG flow on a LoCoMo example

Question: "What did Caroline research?" (single-hop; the answer is in a single fact chunk about Caroline).

```
1. Vector search (seed chunks):
   seed = [
     {id: "conv-0##15", text: "Caroline: I researched adoption agencies for my family.", score: 0.92, triplets: [(Caroline, researched, adoption agencies)]},
     {id: "conv-0##42", text: "Caroline: I'm a PhD student in sociology.",                  score: 0.78, triplets: [(Caroline, is, PhD student)]},
   ]

2. KG expansion (one hop):
   - From "Caroline" → find other chunks mentioning Caroline
   - expanded = seed + 5 more chunks (all mentioning Caroline)
   - Triplets are NOT used to expand (the chunks already mention Caroline)

3. Graph filter:
   - Build chunk graph (no edges between chunks here; the seed chunks share Caroline as an entity but the triplets don't connect them)
   - Each chunk is its own component; take top-k by score

4. Rerank: cosine (no reranker in v1)

5. Return: top-k chunks, organized into a paragraph
   "Caroline researched adoption agencies for her family (conv-0##15). Caroline is a PhD student in sociology (conv-0##42). ..."
```

Question: "How is Caroline connected to the Q3 financial report?" (multi-hop; requires bridging through entities).

```
1. Vector search (seed chunks):
   seed = [
     {id: "conv-0##15", text: "Caroline: I researched adoption agencies for my family.", triplets: [(Caroline, researched, adoption)]},
     {id: "conv-3##100", text: "Adoption Agency X's Q3 report shows revenue of $1M.",        triplets: [(Adoption Agency X, has report, Q3 financial report)]},
   ]

2. KG expansion (one hop):
   - From "adoption" (in chunk 1) → chunks mentioning "adoption"
   - "Adoption Agency X" (in chunk 2) → chunks mentioning "Adoption Agency X"
   - Bridge: chunk 1 mentions "adoption" (entity); chunk 2 mentions "Adoption Agency X" (entity); these are different entities
   - **No automatic bridge.** The expansion is one-hop, not two-hop. The agent gets both chunks and synthesizes.

3. Graph filter:
   - The two seed chunks are not directly connected (no shared triplet entity)
   - But the chunks COULD be connected if we extracted "Caroline" → "Adoption Agency X" as a relation
   - **This is the limit of one-hop KG²RAG.** Two-hop expansion is Layer 1+.

4. Rerank: cosine (no reranker in v1)

5. Return: top-k chunks, organized into a paragraph
   "Caroline researched adoption agencies (conv-0##15). Adoption Agency X's Q3 report shows revenue of $1M (conv-3##100). [The agent synthesizes: Caroline is connected to the Q3 financial report through the Adoption Agency X.]"
```

**The multi-hop case is where KG²RAG helps.** Without the KG, vector search might not find chunk 3##100 (the Q3 report); with the KG expansion, it's surfaced because the seed chunk mentioned "adoption" and the Q3 report is about "Adoption Agency X". The agent then reads both and reasons.

---

## Appendix B — The gRPC surface (D10, preserved)

The `MemoryProvider` gRPC service (D10 from the prior ADR) is preserved. The kernel reads chunks + chunk_triplets instead of typed documents + document_edges. The gRPC surface is unchanged:

```protobuf
service MemoryProvider {
  rpc Ask(AskRequest) returns (AskResponse);
  rpc RecallFacts(RecallRequest) returns (RecallFactsResponse);
  rpc RecallEntities(RecallRequest) returns (RecallEntitiesResponse);
  rpc RecallTopics(RecallRequest) returns (RecallTopicsResponse);
  rpc RecallByID(RecallByIDRequest) returns (NodeResponse);
  rpc RecallPath(RecallPathRequest) returns (PathResponse);
  rpc RecallAtTime(RecallAtTimeRequest) returns (RecallFactsResponse);
  rpc Remember(RememberRequest) returns (RememberResponse);
}
```

The SDK is ~30 lines of gRPC + dataclass marshalling. See ADR-0053 v1 Appendix B for the Python shape.

---

## Appendix C — The testability win (D10, preserved)

The kernel-side `MemoryProvider` is in-process testable. The `KG2RAGRetrieve` function takes fakes for `embedder`, `vectorStore`, `chunkTripletsStore`; no gRPC, no SDK, no real LLM. Tests run in microseconds. See ADR-0053 v1 Appendix C for the test pattern.

The new tests for Phase 0:
- `TestKGExpand_SingleHop` — vector + KG finds chunks that share entities
- `TestKGExpand_NoTriplets` — chunk with no triplets returns just itself
- `TestKGExpand_OneHopLimit` — does NOT cross to two-hop (Layer 1+ adds two-hop)
- `TestKGExpansion_BetterThanVectorOnly` — on a synthetic multi-hop question, KG expansion beats vector-only
- `TestChunkTriplets_LLMParser` — parse `<h##r##t>$$<h##r##t>` output, normalize entities
- `TestChunkTriplets_DedupByKey` — same (chunk, h, r, t) tuple is idempotent
- The LoCoMo benchmark is the integration test (expected lift on multi-hop + single-hop)

---

## What this ADR is NOT

- This ADR is **not** a proposal for a 4-type / 7-predicate knowledge graph. That was the prior version, now superseded.
- This ADR is **not** a proposal for a Wikidata-style 2+2 model. That was also superseded.
- This ADR is **not** a complete company brain. The 5 layers (bi-temporal, event-sourced, community detection, recency-aware, aggregation) are deferred.

This ADR is the **foundation**: the chunks model, the per-chunk triplets, the KG²RAG retrieval pipeline, and the kernel-side provider. **This is what we build now.** The layers are added on top as we need them.
