# ADR-0017: Engram Engine — Spreading Activation Layer

**Status:** Implemented
**Date:** 2026-05-17
**Deciders:** Afsin, Claude

**Prerequisites:** ADR-0015 (Engram Engine — LTM layer), ADR-0016 (Global Workspace Stage — WorkspaceStage integration point)

**Implementation Dependency Graph:**

Hard dependencies on ADR-0015:
- `document_edges` schema (requires `documents` table to exist with FK)
- `activation_strength` field (BFS formula uses `activation_strength_j`)
- `DocTypeMnemonicFact` seed-node filter

Hard dependencies on ADR-0016:
- `WorkspaceStage` interface (spreading call site lives inside `WorkspaceStageImpl`)
- Enrichment map metadata format (`[GRAPH_INCOMPLETE]` tag, `[CONFLICT]` tag)

Parallelisable before both prerequisites are live:
- `SpreadingEngine` struct definition
- `GraphStore` interface (`GetAdjacentEdges` slice-only variant)
- CVR integration test (runs against pgvector test container — no production dependencies)
- `ConsolidatorAgent` stub and edge extraction prompt

Blocked until:
- `document_edges` migration applied (ADR-0015 migration sequence)
- `WorkspaceStage` interface stable (ADR-0016 slice 0016-01)

---

## Context

ADR-0015 established a pgvector LTM with lifecycle-based `activation_strength`. ADR-0016 added the `WorkspaceStage` interface to enrich the Planner and DAGExecutor with cross-session LTM facts. Both layers operate on flat semantic similarity — pgvector cosine distance surfaces documents that are *semantically similar* to a query, but cannot surface documents that are *causally related* to it.

Causal relationships are the critical gap for multi-source reasoning:

- A GitHub PR that *closes* a Linear ticket is not semantically similar to that ticket — it uses different vocabulary. Cosine distance will not reliably surface the PR when querying the ticket's domain.
- A Slack thread that *contradicts* an architectural decision recorded in a Notion doc may use entirely different terminology. The contradiction is invisible to a flat similarity search.
- A design document that *specifies* the implementation now materialised in a PR will not be retrieved by querying the PR's diff text.

Without explicit causal edges, the spreading activation layer in `WorkspaceStage` returns only semantically proximate documents. The graph layer closes this by encoding directed relationships between documents as typed, weighted edges — allowing activation energy to flow along causal paths that cosine distance cannot traverse.

### Ingestion Architecture

Enterprise tool data (Slack, Linear, GitHub, Notion, call transcripts) enters Cambrian through **external optional agents** — registered Python agents in the standard agent registry that call `Server.Execute` to process and ingest tool-specific content. This is a deliberate architectural choice consistent with the Zero-Hardcode Rule (ADR-0001) and the A2A external agent model (ADR-0006). There are no internal Go adapter connectors for external tools. Each tool integration is an independently deployable, optionally registered agent that produces step results flowing through the standard two-tier write pipeline (ADR-0015). The graph layer enriches automatically from whatever those agents produce.

**Design Philosophy Note:** The biological mechanisms referenced in this ADR (spreading activation, associative energy propagation, BFS over causal graph) are architectural constraints that define the solution space, not decorative metaphors. Collins & Loftus (1975) demonstrated that activating one concept in semantic memory causes activation to spread along associative links to related concepts, priming their retrieval even before a direct query. The `SpreadingEngine` implements this faithfully: a pgvector cosine hit is the priming cue; the BFS is the spreading phase; `activation_strength_j` modulates amplitude at each node. This ADR assumes the biological-grounding philosophy is accepted; its purpose is to specify the implementation and its empirical validation path (CVR integration test, traversal logging), not to re-justify the philosophy.

---

## Decisions

### 1. Native PostgreSQL Graph via `document_edges` Table

No external graph database. The existing PostgreSQL/pgvector instance gains a `document_edges` table:

```sql
CREATE TABLE document_edges (
    source_id  TEXT NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    target_id  TEXT NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    edge_type  VARCHAR(50) NOT NULL,
    weight     REAL NOT NULL DEFAULT 0.5,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY (source_id, target_id, edge_type)
);

CREATE INDEX idx_doc_edges_source ON document_edges(source_id);
CREATE INDEX idx_doc_edges_target ON document_edges(target_id);
```

The composite primary key `(source_id, target_id, edge_type)` allows multiple typed edges between the same pair of documents (e.g., a PR that both `closes` a ticket and is `discussed_in` the same Slack thread). Bidirectional indexes support both outbound and inbound traversal at query time.

Migration filename: `db/migrations/NNN_add_document_edges.sql` where NNN is the next sequential number after ADR-0015's migrations. Applied after ADR-0015 Phase 1 is live.

### 2. EdgeType Taxonomy — Four Types, SemanticJump Dropped

```go
type EdgeType string

const (
    EdgeCloses      EdgeType = "closes"       // PR/commit closes a ticket or issue
    EdgeSpecifies   EdgeType = "specifies"    // design doc specifies an implementation
    EdgeContradicts EdgeType = "contradicts"  // fact contradicts another fact
    EdgeDiscussedIn EdgeType = "discussed_in" // artifact referenced in a discussion thread
)
```

`EdgeSemanticJump` is explicitly not included. Semantic proximity is already handled by pgvector cosine distance in the base hit phase. Storing semantic edges explicitly would scale as O(N²) over the document corpus and duplicate what pgvector already does. The four explicit types represent relationships that cosine distance cannot infer — they require either pattern-matched references or LLM-reasoned intent.

### 3. Separate `GraphStore` Interface — Not a VectorStore Extension

`VectorStore` operates on the `documents` table (vectors, metadata, access counts). `GraphStore` operates on the `document_edges` table. Mixing them would break all existing `VectorStore` mock implementations in the test suite.

A new interface in `internal/domain/`:

```go
type GraphStore interface {
    SaveEdge(ctx context.Context, edge *DocumentEdge) error
    GetAdjacentEdges(ctx context.Context, docIDs []string) ([]DocumentEdge, error)
    UpdateEdgeWeight(ctx context.Context, sourceID, targetID string, edgeType EdgeType, newWeight float32) error
}
```

`GetAdjacentEdges` accepts a slice of document IDs — not a single ID. The BFS traverses one depth level at a time; batching all seed nodes for a given depth into a single `WHERE source_id = ANY($1)` query reduces the round-trip count from O(b³) to O(MaxDepth) — for MaxDepth=3, this is 3 queries instead of potentially hundreds. No single-node variant is exposed; the slice-only contract prevents the per-node query anti-pattern.

`PgVectorAdapter` satisfies both `VectorStore` and `GraphStore` — they share the same `*pgxpool.Pool`. `SpreadingEngine` and `ConsolidatorAgent` receive `GraphStore` as an injected field. `GetByID` already exists on `VectorStore` — no new document hydration method needed.

### 4. `Document.Links []string` Replaced Immediately

`Document.Links []string` ("IDs of connected documents (GraphRAG lineage)") is removed from the domain type. It is a denormalized adjacency list with no edge type, no weight, and no `created_at` — strictly inferior to `document_edges`. The `links []string` parameter is removed from `Agent.IngestSync`; edge writing becomes a separate `GraphStore.SaveEdge` call after document save, separating the two concerns. All callers of `IngestSync` are updated as part of ADR-0017 implementation slice 0017-01.

### 5. Type-Specific Default Edge Weights — Bootstrap Hypotheses

Different edge types carry different epistemic significance. Initial weights reflect this:

| EdgeType | Default Weight | Rationale |
|---|---|---|
| `contradicts` | 0.9 | Strongest signal — explicit conflict; dominates spreading |
| `specifies` | 0.7 | Direct intentional relationship (design → implementation) |
| `closes` | 0.6 | Factual closure — concrete but narrower semantic scope |
| `discussed_in` | 0.5 | Weakest — associative reference only |

**These weights are bootstrap hypotheses, not calibrated constants.** They reflect domain intuition about epistemic strength ordering (`contradicts` > `specifies` > `closes` > `discussed_in`) but have not been validated against plan outcomes. Edge traversal logging (see Section 7) provides the training data for future weight learning. All four defaults are configurable via `GraphConfig`; operators who observe unexpected spreading behaviour should adjust via config before concluding the model is wrong.

All four defaults are configurable via `GraphConfig` (see Decision 11). `ConsolidatorAgent.ExtractGraphEdges` sets the correct initial weight at write time based on the detected edge type.

### 6. SpreadingEngine — Corrected BFS Formula

The spreading engine lives in `internal/memory/spreading_engine.go`. It performs bounded BFS over the `document_edges` graph, propagating activation energy from the initial pgvector hit set.

**Corrected activation formula:**

```
A_j = (BaseCosine_j + Σ_{i ∈ inbound}(A_i · w_ij)) · d^depth · activation_strength_j
```

Where:
- `BaseCosine_j` — cosine similarity from the pgvector base hit (0.0 for graph-discovered nodes not in the initial hit set)
- `w_ij` — `document_edges.weight` for the edge from i to j
- `d` — `GraphConfig.DecayFactor` (default 0.75) — attenuation per hop
- `depth` — BFS hop count from the nearest seed node
- `activation_strength_j` — from `documents.activation_strength` (ADR-0015); encodes cross-session retrieval history and Ebbinghaus decay. High-access mature memories amplify spreading energy; stale unaccessed memories dampen it.

**Why `activation_strength_j` instead of a temporal decay multiplier `1/(1+λΔt)`:**
A temporal multiplier would double-count the age signal already encoded in `activation_strength` via ADR-0015's nightly Ebbinghaus decay. A foundational architectural decision made 18 months ago that the team keeps referencing has *high* `activation_strength` — a temporal multiplier would suppress it. `activation_strength` correctly encodes age modulated by access frequency; `1/(1+λΔt)` encodes raw age alone.

**BFS batch execution:** `GetAdjacentEdges` is called once per depth level with all nodes at that depth as a batch (`WHERE source_id = ANY($1)`). This is O(MaxDepth) queries per spread call, not O(nodes_visited). At MaxDepth=3 with branching factor b, the unbatched approach issues b³ queries; the batched approach issues 3.

**Operating assumption:** Seed nodes sourced from the pgvector base hit are expected to have `activation_strength ≥ 0.3` in steady-state operation (they were retrieved by cosine similarity, meaning they have been accessed enough times to mature toward the floor of the retrieval-viable range). BFS graph-discovered nodes may have lower AS; the formula naturally attenuates energy through them. Telemetry field `spreading_node_as_p50` records the median `activation_strength` across all nodes that contributed to the final spread result. SPC alert threshold: if `spreading_node_as_p50 < 0.2` over a 7-day window, the operating assumption is violated — the graph is being traversed through stale, unvalidated documents, which produces noisy enrichment context.

**Cycle fix:** A `dequeued map[string]bool` set (not `visitedDepth`) prevents re-processing. A node is added to `dequeued` when it is first popped from the queue — not when first encountered. This prevents energy accumulation on cyclic graphs where `existingEnergy < transferredEnergy` would otherwise re-queue and re-energise already-processed nodes.

BFS terminates when `depth >= GraphConfig.MaxDepth` OR `currentEnergy < GraphConfig.EnergyFloor`.

**Graph Coverage Guard:** For every `SpreadingEngine.Spread` call, compute `edge_coverage = (seed nodes with ≥1 outgoing edge) / total seed nodes`. The coverage ratio gates BFS execution and Planner disclosure:

| Coverage | Behaviour |
|---|---|
| `= 0` | Return flat cosine hits only; log `bfs_graph_miss=true` |
| `0 < coverage < 0.5` | Execute BFS; append `[GRAPH_INCOMPLETE]` to enrichment map metadata; log `bfs_graph_partial=true, edge_coverage=ratio`; Planner prompt includes: *"Cross-vocabulary causal links may be incomplete; verify multi-source facts independently."* |
| `≥ 0.5` | Normal BFS execution |

The empty-graph case (coverage = 0) is distinct from the partial-graph case. The dangerous failure mode is partial staleness: old edges are traversed while newer causal links are absent, giving the Planner false confidence in cross-vocabulary completeness. The `[GRAPH_INCOMPLETE]` tag makes partial staleness visible rather than silent.

### 7. Split Edge Write Path — Synchronous + Deferred

Edge writing is split by detection cost:

**Synchronous during `MemoryAgent.RecordExecution`:**
Pattern-matched explicit references only — ticket IDs (`ENG-\d+`, `#\d+`), PR URLs, document reference patterns detectable via regex. Edge types: `closes`, `discussed_in`. Cost: one regex scan + one `INSERT INTO document_edges` per relevant step output. No LLM call. Edges are available for spreading in the next `Execute` call within minutes of being produced.

**Deferred to `ConsolidatorAgent.ExtractGraphEdges` at consolidation time:**
Semantic relationships requiring LLM reasoning: `specifies`, `contradicts`. The Generator receives a batch of session events and produces a structured `[]DocumentEdge` JSON array. These edges are written after the LLM call completes at 03:00 consolidation. Not on the hot path.

**ConsolidatorAgent timeout constraint:** The LLM call in `ExtractGraphEdges` MUST use `context.WithTimeout(ctx, cfg.ConsolidatorLLMTimeout)` where `consolidator_llm_timeout` defaults to 60s in `ExecutionConfig`. On timeout: skip the batch, log `consolidator_timeout=true, batch_size=N`. Do NOT update `last_consolidation_run_at`. CircadianRhythm's staleness check will surface the lag on next startup. Default 60s (vs. Tier-2's 30s) because ConsolidatorAgent processes a full session narrative to extract semantic relationships — a more complex generation task than Tier-2's constrained 3-score schema.

**ConsolidatorAgent staleness detection:** `ConsolidatorAgent` writes a `last_consolidation_run_at` timestamp to a `system_state` metadata row after each successful edge extraction batch. `CircadianRhythm` checks this timestamp on startup and logs `graph_staleness_hours=N` if `last_consolidation_run_at` is more than 26 hours old (24h cycle + 2h grace). SPC alert threshold: >48h staleness triggers an operator ticket. The staleness timestamp is a **sensor only** — it does not trigger any automatic circuit-breaking or BFS disable. Staleness > 48h means `CircadianRhythm` surfaces a warning and the operator decides whether to manually trigger consolidation; no code path change fires automatically.

Rationale: synchronous writes for pattern-detected references ensure the graph is populated progressively — not purely overnight. The LLM-required semantic edges (specifies, contradicts) are deferred because they cannot be detected reliably by pattern matching and must not block the execution path.

**Edge traversal logging (Thompson Sampling seed):** When a BFS-discovered node enters the final enrichment set (i.e., the node passes the `activation_strength` operating assumption and contributes to `ltm_*` keys), log: `edge_type`, `edge_weight`, `source_doc_id`, `target_doc_id`. This structured log is the precondition for future Thompson Sampling-based edge weight learning — it records which edge types and weights actually contributed to enrichment context, enabling reward linkage once plan outcome data is available.

### 8. `contradicts` Edge Weight Decay on Resolution

When `ConsolidatorAgent.BuildReconsolidationPrompt` (ADR-0016) resolves a contradiction and selects a winner:

```go
store.UpdateEdgeWeight(ctx, sourceID, targetID, EdgeContradicts, existingWeight * 0.5)
```

The losing document's `contradicts` edge weight is halved. After two resolutions against the same loser, the edge weight drops below 0.25 and contributes negligible spreading energy. The edge is retained (for audit history) but effectively silenced. This prevents a resolved contradiction from continuing to activate the loser document indefinitely.

**This ×0.5 schedule is a bootstrap hypothesis, not a calibrated decay curve.** Observable: if `contradicts`-edge-originated nodes in the enrichment map include documents whose associated contradictions have been resolved >2 times (edge weight < 0.25), the ×0.5 schedule is too gentle and should be increased toward ×0.25. Edge traversal logs provide the data to observe this.

### 9. `WorkspaceStage` Integration

`SpreadingEngine.Spread` is called inside `WorkspaceStageImpl.PrimeForPlanning` and `PrimeForExecution` (ADR-0016) after the base pgvector hit:

```
1. Embed query (Embedder.Embed)
2. SCENE retrieval → primed embedding (ADR-0016)
3. Base pgvector FACT hit (top workspace_planning_slots results)
4. SpreadingEngine.Spread(baseHits) → expanded GraphNodeExpansion set
5. Sort by ActivationEnergy → serialize top-k to map[string]string slots
```

The spreading step adds graph-discovered documents not returned by cosine similarity alone. The final slot count respects `workspace_planning_slots` / `workspace_execution_slots` (ADR-0016 config).

### 10. `SearchOptions.Since` Temporal Filter

```go
type SearchOptions struct {
    DocumentType string
    TopK         int
    Filter       string
    Since        time.Time // zero value = no filter; maps to WHERE created_at > $1
}
```

Closes the temporal scope query gap: "what did the team decide in Q1" is now a first-class query. Zero value means no filter — fully backward compatible.

### 11. Nested `GraphConfig` in `ExecutionConfig`

Seven parameters warrant their own namespace:

```go
type GraphConfig struct {
    DecayFactor          float64 `json:"decay_factor"`           // default: 0.75
    MaxDepth             int     `json:"max_depth"`              // default: 3
    EnergyFloor          float64 `json:"energy_floor"`           // default: 0.15
    WeightContradicts    float64 `json:"weight_contradicts"`     // default: 0.9
    WeightSpecifies      float64 `json:"weight_specifies"`       // default: 0.7
    WeightCloses         float64 `json:"weight_closes"`          // default: 0.6
    WeightDiscussedIn    float64 `json:"weight_discussed_in"`    // default: 0.5
    ConsolidatorLLMTimeout int   `json:"consolidator_llm_timeout"` // default: 60 (seconds)
}
```

Added as `Graph GraphConfig \`json:"graph"\`` on `ExecutionConfig`. Config file entry:

```json
"graph": {
  "decay_factor": 0.75,
  "max_depth": 3,
  "energy_floor": 0.15,
  "weight_contradicts": 0.9,
  "weight_specifies": 0.7,
  "weight_closes": 0.6,
  "weight_discussed_in": 0.5,
  "consolidator_llm_timeout": 60
}
```

---

## §7.1 Cross-Vocabulary Recall Integration Test (Mandatory — Slice 0017-08)

ADR-0017's central claim is that BFS surfaces causally-linked documents which cosine distance misses due to vocabulary mismatch. This claim is validated by an automated proxy benchmark before deployment — no human relevance labels required.

**Test Protocol (Cross-Vocabulary Recall — CVR):**

1. Seed a test `documents` table with N=50 documents, partitioned into 10 domain clusters (e.g., "auth-service", "payments", "frontend").
2. Create `document_edges` linking cross-vocabulary pairs: a document in cluster A (embedding centroid far from cluster B) linked by a `specifies` edge to a document in cluster B.
3. Execute 10 test queries, each targeting one document in cluster B using its exact text.
4. **Metric 1 (Flat Baseline):** Fraction of cross-linked cluster-A documents appearing in top-5 cosine hits. Expected: ≤0.1 (cosine should miss them due to vocabulary gap).
5. **Metric 2 (BFS Treatment):** Fraction of cross-linked cluster-A documents appearing in the final `SpreadingEngine.Spread` result set. Expected: ≥0.7.
6. **Pass criterion:** `CVR = Metric2 - Metric1 ≥ 0.5`. If CVR < 0.5, the spreading engine is not bridging the vocabulary gap — ADR-0017 implementation is incomplete.

**Why this is the right proxy:**
- Requires zero human labels — ground truth is the injected edge structure.
- Tests the specific failure mode ADR-0017 claims to solve (cross-vocabulary retrieval).
- Automated and repeatable — runs in CI on every build against a pgvector test container.
- Produces a scalar score (CVR) trackable over code changes.

**SPC link:** The CVR test establishes the synthetic baseline. Once production traversal logs exist (Section 7 edge traversal logging), a future ADR (P2) will measure whether CVR correlates with real-world plan quality. If it does not, the synthetic test is miscalibrated and must be updated.

**Location:** `internal/memory/spreading_engine_test.go`. Runs against a temporary pgvector test container. This test is **mandatory** and blocks ADR-0017 acceptance — it is not optional.

---

## Considered Options

**External graph database (Neo4j, ArangoDB).** Adds operational complexity, a new infrastructure dependency, and a cross-database join problem (pgvector documents + Neo4j nodes). Native PostgreSQL eliminates the join — `document_edges` rows reference `documents.id` directly with foreign key enforcement. Rejected.

**`EdgeSemanticJump` type.** Semantic proximity is already handled by pgvector cosine distance. Storing semantic edges explicitly scales as O(N²) and duplicates the base hit phase. Rejected.

**Extend `VectorStore` interface.** Graph operations query a different table with different semantics. Adding them to `VectorStore` breaks all existing mock implementations and conflates two distinct responsibilities. Rejected.

**Deprecate `Document.Links []string` gradually.** Keeping both `Links` and `document_edges` requires dual-write logic and leaves a field that new contributors would inevitably write to. Immediate removal is cleaner. Rejected.

**Flat fields in `ExecutionConfig` with `graph_` prefix.** Seven+ fields is the natural breakpoint for a nested struct. Flat fields would make `ExecutionConfig` a kitchen sink. Rejected.

**Internal Go ingestion adapters for external tools.** Ingestion of Slack, Linear, GitHub, Notion data is handled by external optional agents registered in the standard agent registry — consistent with the Zero-Hardcode Rule and ADR-0006 A2A model. No internal adapter code is needed or appropriate. Rejected.

**Temporal decay multiplier `1/(1+λΔt)` in spreading formula.** Double-counts the age signal already encoded in `activation_strength` via ADR-0015's Ebbinghaus decay. A foundational decision with many retrievals has high `activation_strength` and would be incorrectly suppressed by a raw time multiplier. Rejected; `activation_strength_j` incorporated into the spreading formula instead.

**BFS per-node `GetAdjacentEdges` (single-ID variant).** O(b^MaxDepth) SQL queries per spread call. At MaxDepth=3 with branching factor b=5, this is 125 queries per spread call. Replaced by slice-only `GetAdjacentEdges(docIDs []string)` with `WHERE source_id = ANY($1)` — O(MaxDepth) queries. Single-ID variant not exposed.

**Thompson Sampling for edge weight learning.** Proposed as P2: learn edge weights based on plan outcome signals via Thompson Sampling. Valid direction — the static weights (0.9/0.7/0.6/0.5) are bootstrap hypotheses. Deferred: Thompson Sampling requires a reward signal linked to specific edge traversals (which edge → which document → which plan outcome). The current architecture does not log this linkage. Edge traversal logging (Section 7) is the precondition. A future ADR will build the sampling loop once the reward plumbing is in place.

**Offline nDCG@K retrieval benchmark as P0 prerequisite.** Proposed as a prerequisite before implementing ADR-0017: build an offline benchmark with labeled relevance judgments. Re-graded to P2: nDCG@K requires labeled queries (for query Q, document D has relevance grade G). Cambrian has no such labels pre-production. The CVR proxy benchmark (§7.1) validates the mechanism without labels; nDCG@K is buildable once ≥100 sessions of retrieval logs with linked plan outcomes exist.

**Circuit-breaker on graph staleness.** Proposed to disable BFS entirely when `last_consolidation_run_at` exceeds 48h. Rejected: a secondary health issue (consolidation delay) should not mutate into a primary retrieval regression (all cross-vocabulary links vanish). The Graph Coverage Guard already handles the empty-graph and partial-graph cases gracefully. The staleness timestamp is a sensor for operators, not an actuator for automatic BFS disable.

---

## Consequences

- `internal/domain/` gains: `DocumentEdge`, `EdgeType` constants, `GraphStore` interface (slice-only `GetAdjacentEdges`), `GraphNodeExpansion`; `SearchOptions.Since time.Time` added; `Document.Links []string` removed.
- `internal/memory/` gains: `SpreadingEngine` with corrected BFS formula, batch-per-depth query, Graph Coverage Guard; `WorkspaceStageImpl` updated to call `Spread` after base hit.
- `internal/awareness/consolidator_agent.go` gains: `ExtractGraphEdges` for LLM-based semantic edge detection (`specifies`, `contradicts`); 60s timeout via `ConsolidatorLLMTimeout`.
- `internal/memory/agent.go`: `IngestSync` signature loses `links []string`; gains synchronous pattern-matched edge writing via `GraphStore.SaveEdge`; gains edge traversal logging.
- `internal/config/config.go`: `GraphConfig` nested struct with 8 fields (7 parameters + `ConsolidatorLLMTimeout`); `ExecutionConfig.Graph GraphConfig` field; `configs/config.json` gains `"graph"` block.
- `PgVectorAdapter` implements `GraphStore` alongside existing `VectorStore`.
- `db/migrations/NNN_add_document_edges.sql` — applied after ADR-0015 migrations.
- All existing `VectorStore` mock implementations are unaffected.
- ADR-0015 must be fully deployed (`activation_strength` column live) before `SpreadingEngine` can incorporate `activation_strength_j` into energy calculation.
- `CircadianRhythm` checks `last_consolidation_run_at`; logs `graph_staleness_hours=N`; SPC alert at >48h (operator action only, no automatic behavior change).
- Structured log fields emitted per spread call: `bfs_graph_miss`, `bfs_graph_partial`, `edge_coverage`, `spreading_node_as_p50`.
- Edge traversal log fields per contributing edge: `edge_type`, `edge_weight`, `source_doc_id`, `target_doc_id` — Thompson Sampling seed data.
- Thompson Sampling-based edge weight learning deferred to a future ADR; trigger: reward linkage (edge traversal → plan outcome) must be instrumented first.
- CVR integration test (`internal/memory/spreading_engine_test.go`) is mandatory — blocks acceptance of ADR-0017. Pass criterion: `CVR ≥ 0.5`.

---

## Implementation Slices

| Slice | Description | Blocked by |
|-------|-------------|------------|
| 0017-01 | Domain layer: `DocumentEdge`, `EdgeType`, `GraphStore` interface (slice-only `GetAdjacentEdges`), `GraphNodeExpansion`; `SearchOptions.Since`; remove `Document.Links`; update `IngestSync` | ADR-0015 Phase 1 |
| 0017-02 | `PgVectorAdapter` implements `GraphStore`; `document_edges` migration | 0017-01 |
| 0017-03 | `SpreadingEngine` BFS with corrected formula; batch-per-depth query; Graph Coverage Guard; cycle fix; unit tests (attenuation, cycle, energy floor, coverage bands) | 0017-02 |
| 0017-04 | `WorkspaceStageImpl` integration: spread after base hit in both `PrimeForPlanning` and `PrimeForExecution`; `GraphConfig` + kernel wiring | 0017-03 |
| 0017-05 | Synchronous pattern-matched edge writing in `MemoryAgent.RecordExecution` (`closes`, `discussed_in`); edge traversal logging | 0017-02 |
| 0017-06 | `ConsolidatorAgent.ExtractGraphEdges` for LLM-based `specifies`/`contradicts` edges; `contradicts` weight decay on resolution; 60s timeout; staleness timestamp | 0017-04, 0017-05 |
| 0017-07 | Integration test: cross-source synthesis (seed two documents, link via edge, verify spreading surfaces both from single-domain query) | 0017-06 |
| 0017-08 | **CVR integration test** (mandatory, blocks acceptance): N=50 docs, 10 clusters, cross-vocabulary edge pairs, CVR ≥ 0.5 pass criterion; runs in CI against pgvector test container | 0017-03 |
