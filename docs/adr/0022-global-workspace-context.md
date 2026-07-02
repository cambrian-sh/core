# ADR-0022: Global Workspace Context — Capacity-Limited Working Memory with Predictive Coding

**Status:** Accepted (grilling session 2026-05-29 — 14 decisions resolved)  
**Date:** 2026-05-29  
**Deciders:** Afsin  
**Depends on:** ADR-0016 (WorkspaceStage — enrichment layer), ADR-0021 (System Quality Measurement)
**Precedes:** ADR-0023 (if accepted) — Consolidation & Interleaved Replay  

---

## ⚠️ Executive Summary

This ADR replaces the eager, unbounded `map<string, string> context` in `Handoff` with a **Global Workspace** (`[]ContextRef`) where each ref is self-contained (`cid + type + labels + activation + precision + snippet`). Context selection uses **spreading activation** (structural graph relevance) and **precision** (semantic cosine relevance). Baddeley-style buffer routing was considered and removed — modality is implicit in `Payload.type` and `ContextRef.labels`.

**Status note:** ADR-0015 (Engram Engine), ADR-0016 (WorkspaceStage), and ADR-0017 (SpreadingActivation) are all **fully implemented** as of 2026-05-29 — contrary to what CONTEXT.md stated before this session. The graph is live. The phasing constraint "wait for lower layers" no longer applies. Implementation can proceed immediately in phase order.

---

## ⚠️ Critical Correction: CAS, Not Merkle DAG

The original draft described Layer 2 as a "Merkle DAG." This was incorrect. A true Merkle DAG requires parent CIDs to be computed from child CIDs (`Hash(parent_data + child_CIDs)`), creating cryptographic integrity chains. In a parallel DAG executor where steps complete out of order and replanning rewrites prior results, a Merkle structure would trigger **cascade invalidation** — every ancestor node's CID changes when a child is revised, invalidating all existing `ContextRef` pointers.

**Layer 2 is a Content-Addressed Store (CAS):** `CID = Hash(data)` independently of parent relationships. Provenance edges (`Parents []CID`) are stored as metadata, not as part of the hash. This gives us deduplication and auditability without cascade invalidation. The terminology throughout this ADR has been corrected to CAS.

---

## 1. Problem Statement

### 1.1 The Current Context Is a Monolithic String Dump

`Handoff.Context` is `map<string, string>`. Every step receives a `cloneMap(snapshot)` containing:
- All prior `step_N_result` strings
- All prior `step_N_{k}` agent-added keys
- All `ltm_*` keys from WorkspaceStage enrichment (ADR-0016)

For a 10-step plan with 500-token results, step 9 receives **~4,500 tokens of prior context** before its own query is even added. This is the O(N²) token growth we observed in ADR-0021 benchmarks.

### 1.2 The Current Context Is Untyped and Unstructured

A `map<string, string>` cannot distinguish:
- A step result (episodic, short-lived)
- An LTM document (semantic, persistent)
- A verification score (meta-data, tiny)
- A code snippet (structured, needs syntax highlighting)
- An image embedding (binary, not text)

Everything is flattened to a string. Agents receive a wall of text with no semantic markers about what each piece means or how much to trust it.

### 1.3 The Current Context Is Not Content-Addressed

If step 3 and step 7 both produce the same output (e.g., "The answer is 42"), the executor stores two copies in `masterContext`. There is no deduplication. Cross-plan sharing is impossible — each plan's `masterContext` is an isolated island.

**Note on cross-plan scope after this ADR:** CAS enables deduplication within a plan and across *concurrently executing* plans. Sequential plan deduplication is limited by the plan-scoped GC decision (Section 2.3, Q11): when Plan A's `Execute()` returns and calls `GC(planACIDs)`, Plan B has not yet started, so no CIDs are shared. Cross-plan sharing for sequential plans requires session-scoped GC, which is deferred to ADR-0023 (see `docs/future_work_summary.md`). **Do not claim global cross-plan deduplication from Phase 1 alone.**

### 1.4 The Current Context Violates Every Theory in docs/theory/

| Theory | What It Says | What Cambrian Does Instead |
|--------|-------------|---------------------------|
| **Global Workspace Theory** (Baars, 1988) | Working memory is capacity-limited (~7±2 items); a spotlight of attention selects what enters conscious awareness | Every step receives **all** items; no capacity limit; no attentional gating |
| **Predictive Coding / FEP** (Friston, 2010) | Ascending connections carry **only prediction errors** — the information the higher layer failed to predict | The executor passes **raw data upward** (full prior results) instead of passing residuals |
| **Baddeley & Hitch** (1974) | Working memory is **not a single buffer**; it has domain-specific slave systems (phonological loop, visuospatial sketchpad, episodic buffer) | `map<string, string>` is a single monolithic buffer for all modalities |
| **Spreading Activation** (Collins & Loftus, 1975) | Activation energy spreads from a query node through a graph, decaying with distance; only high-activation nodes reach awareness | Context selection uses flat embedding similarity with no graph propagation or decay |
| **Complementary Learning Systems** (McClelland et al., 1995) | Fast episodic traces (hippocampus) consolidate into slow semantic memory (neocortex) through **interleaved offline replay** | Step results go to bbolt and die; no consolidation pathway to LTM |

---

## 2. Decision

We will replace the flat `map<string, string> context` with a **Global Workspace** architecture composed of four interacting layers.

### 2.1 The Four Layers

```
┌────────────────────────────────────────────────────────────────────────┐
│  LAYER 4: GLOBAL WORKSPACE — Activation-threshold working set          │
│  []ContextRef working_memory  (all refs ≥ activation_threshold=0.3)   │
│  Each ContextRef: cid + type + labels + activation + precision + snippet│
│  Hard ceiling: max_context_slots=20 (graph explosion guard)            │
│  BufferType removed — modality via Payload.type / ContextRef.labels   │
└──────────────────────────┬─────────────────────────────────────────────┘
                           │
              ┌────────────┴──────────────┐
              │                           │
         ┌────▼────┐                 ┌────▼────┐
         │ LAYER 3 │                 │ LAYER 3 │
         │Spreading│                 │Precision│
         │Activation│                │Weighting│
         │(BFS graph)│               │(cosine) │
         └────┬────┘                 └────┬────┘
              │   both live in            │
              │   WorkspaceStage          │
              │   PrimeForStep()          │
    ┌─────────▼───────────────────────────▼──────┐
    │  LAYER 2: HybridContentStore (CAS)          │
    │  CID = SHA-256(data) — independent of graph │
    │  BBolt backend (< 64KB) + filesystem (≥ 64KB)│
    │  Put/Get/Has/GC — plan-scoped eviction       │
    └──────────────────┬──────────────────────────┘
                       │
    ┌──────────────────▼──────────────────────────┐
    │  LAYER 1: PERSISTENT STORAGE                │
    │  BBolt (plan traces) + pgvector (LTM)        │
    │  + document_edges (graph) — all live         │
    └─────────────────────────────────────────────┘
```

### 2.2 Layer 1: Persistent Storage

No change from ADR-0015/0016:
- **Episodic:** BBolt `content_store` bucket (new) holds plan traces as `ContextNode` records
- **Semantic:** PostgreSQL pgvector holds document embeddings + metadata
- **Graph:** PostgreSQL `document_edges` table (ADR-0017) holds associative links

### 2.3 Layer 2: Content-Addressed Store (CAS)

New interface in `internal/storage/`:

```go
package storage

// CID is a content identifier (SHA-256, base58-encoded).
type CID string

// ContextNode is a single piece of addressable content.
type ContextNode struct {
    CID     CID
    Type    string           // "step_result", "ltm_doc", "verification", "agent_artifact"
    Data    []byte
    Labels  []string         // searchable tags
    Parents []CID            // provenance edges (NOT part of CID computation)
}

// ContentStore is a content-addressed key-value layer.
type ContentStore interface {
    Put(data []byte, nodeType string, labels []string) (CID, error)
    Get(cid CID) (*ContextNode, error)
    Has(cid CID) (bool, error)
    GC(keep []CID) error
}
```

Implementation: `BBoltContentStore` backed by a dedicated bbolt bucket. Same database file as `AgentRegistry` — no new infrastructure.

**Key property:** `Put` computes `CID = SHA-256(data)` independently of `Parents`. If the same data is stored twice, `Has` returns true and no write occurs. Deduplication is automatic. Provenance edges are stored as metadata, not as part of the hash, avoiding Merkle cascade invalidation on replanning.

### 2.4 Layer 3: Three Selection Mechanisms

#### 3A. Spreading Activation (Retrieval from LTM)

**Hexagonal boundary:** Spreading activation lives in `WorkspaceStage.PrimeForStep()`, not in `DAGExecutor`. The executor calls `workspace.PrimeForStep(ctx, query, priorStepRefs, maxItems)` and receives a ranked `[]ContextRef`. The executor does not touch pgvector, embeddings, or `document_edges` directly.

```go
// In internal/memory/workspace_stage.go (Phase 2)
// Phase 2 adds a ContentStore field to WorkspaceStageImpl (injected at wire time).
// Phase 0/1 leave it nil; PrimeForStep degrades gracefully without it.
func (ws *WorkspaceStageImpl) PrimeForStep(
    ctx context.Context,
    query string,          // step.Query — per-step, not plan-level
    priorStepRefs []ContextRef, // CIDs for N ∈ step.DependsOn (passed by executor)
    maxItems int,
) ([]ContextRef, error) {

    queryVec, err := ws.Embedder.Embed(ctx, query)
    if err != nil {
        return nil, err
    }

    // 1. SEED: pgvector ANN search for initial candidates
    seeds, err := ws.Store.Search(ctx, queryVec, domain.SearchOptions{TopK: 20, Floor: ws.RetrievalFloor})
    if err != nil {
        return nil, err
    }

    // 2. PROPAGATION: delegate BFS to SpreadingEngine (ADR-0017).
    // Degrades to seed-only when SpreadingEngine is nil (graph unpopulated in Phase 2).
    var expansions []domain.GraphNodeExpansion
    if ws.SpreadingEngine != nil {
        expansions = ws.SpreadingEngine.Spread(ctx, seeds)
    } else {
        for _, s := range seeds {
            expansions = append(expansions, domain.GraphNodeExpansion{
                Document:         s.Document,
                ActivationEnergy: s.Score,
            })
        }
    }

    // Build seed precision index — cosine score already computed by pgvector.
    // BFS-discovered nodes that didn't appear in seeds don't have a cosine score;
    // they require a separate re-embed + cosine call to compute precision.
    // We defer that cost to assemble_context() in the SDK (fetch_fn path).
    // Here, we record which docs ARE seeds so the SDK can distinguish them.
    seedPrecision := make(map[string]float64, len(seeds))
    for _, s := range seeds {
        seedPrecision[s.Document.ID] = s.Score
    }

    // 3. PRIOR-STEP INJECTION: boost activation for CIDs declared in DependsOn.
    //    Clamped to [0,1]: additive boost must not push activation above 1.0,
    //    which would distort activation×precision scoring in assemble_context().
    priorBoost := make(map[string]float64, len(priorStepRefs))
    for _, ref := range priorStepRefs {
        priorBoost[string(ref.CID)] += 0.3
    }

    // 4. SELECTION: activation-threshold floor + hard ceiling.
    // CRITICAL: do NOT use ws.RetrievalFloor here. RetrievalFloor is a cosine
    // similarity floor for pgvector ANN (reasonable at 0.3). Activation energy
    // for 1-hop BFS nodes is typically 0.15–0.25 — well below RetrievalFloor but
    // a strong structural signal. Using ActivationThreshold (config, default 0.1)
    // keeps BFS-discovered nodes alive. See ML-1 analysis in Flaws section.
    activationThreshold := ws.ActivationThreshold // config.activation_threshold, default 0.1
    if activationThreshold <= 0 {
        activationThreshold = 0.1
    }

    var items []ContextRef
    for _, exp := range expansions {
        energy := min(exp.ActivationEnergy+priorBoost[exp.Document.ID], 1.0)
        if energy < activationThreshold {
            continue
        }
        // Precision: use pgvector cosine score for seeds.
        // BFS-discovered nodes have precision=-1 (sentinel: "not yet computed").
        // assemble_context() in the SDK computes precision lazily via fetch_fn
        // when it needs to rank items — avoiding a re-embed round-trip for every node.
        precision := float32(-1.0) // sentinel: unknown precision
        if p, ok := seedPrecision[exp.Document.ID]; ok {
            precision = float32(p)
        }
        snippet := ""
        if ws.ContentStore != nil { // ContentStore may be nil in Phase 2 dry-run
            if node, err := ws.ContentStore.Get(CID(exp.Document.ID)); err == nil {
                // Only snippet UTF-8 text; skip binary payloads (would be garbled)
                if isUTF8Text(node.Data) {
                    snippet = utf8Truncate(string(node.Data), snippetBytes) // config.context_ref_snippet_chars
                }
            }
        }
        items = append(items, ContextRef{
            CID:        CID(exp.Document.ID),
            Activation: float32(energy),
            Precision:  precision, // -1.0 for BFS nodes; SDK computes lazily
            Snippet:    snippet,
        })
    }
    // Sort by activation only — structural relevance is the primary signal here.
    // Semantic re-ranking (activation×precision) happens in assemble_context()
    // where the SDK has the agent's min_precision threshold and fetch_fn.
    sortByActivation(items)
    if len(items) > maxItems {
        slog.Info("workspace_capacity_truncated", "kept", maxItems, "total", len(items))
        items = items[:maxItems] // hard ceiling: config.max_context_slots
    }
    return items, nil
}
```

**References docs/theory/Spreading_Memory_Theory.md:**
- "Activation energy is a finite resource. When a node fires, its total energy is split among all its outward-facing links."
- "A node can receive small amounts of activation energy from multiple independent directions simultaneously..."

#### 3B. Precision Weighting

Precision (cosine similarity between the step query embedding and the item's cached embedding) is computed **inside `PrimeForStep()`** alongside spreading activation — not as a separate executor pass. Each `ContextRef` carries both signals:

- `activation` = structural relevance (graph topology, BFS propagation)
- `precision` = semantic relevance (embedding cosine similarity)

They are complementary: a heavily-cited document (high activation) may be semantically off-topic (low precision); a semantically matching document may be graph-isolated (low activation). `assemble_context()` sorts by `activation × precision` to prefer items that are both structurally connected and semantically relevant.

**3C. Buffer Router — Removed**

`BufferType` (PhonologicalLoop / VisuospatialSketchpad / EpisodicBuffer) and `selectBuffer()` were in the original draft. **Removed.** Modality is already implicit in `Payload.type` and `ContextRef.labels`. A `code_generation` agent already knows it handles code; adding a proto enum and executor heuristic adds complexity without providing any machine-actionable signal beyond what the capability name encodes. Baddeley's model informs our theoretical framing; it does not need to be serialised into the proto.

### 2.5 Layer 4: The Global Workspace (Handoff)

New proto contract:

```protobuf
// ADR-0022: Replaces monolithic context map with capacity-limited Global Workspace
message ContextRef {
  string cid = 1;                // SHA-256 hex, CAS key
  string type = 2;               // "step_result", "ltm_doc", "verification"
  repeated string labels = 3;    // searchable tags
  float activation = 4;          // spreading activation energy (0-1) — structural relevance
  string snippet = 5;            // first context_ref_snippet_bytes (default 150) of UTF-8 content
                                 // resilience primitive: assemble_context() uses snippet if
                                 // get_context_node() is unavailable (ContentStore hiccup)
  float precision = 6;           // cosine similarity to step query (0-1) — semantic relevance
                                 // complementary to activation: high activation + low precision =
                                 // structurally connected but semantically distant
}

message Handoff {
  string id = 1;
  string from_agent = 2;
  string to_agent = 3;
  Object payload = 4;
  float confidence = 5;
  repeated string uncertainties = 6;
  
  // REMOVED: flat context map. Clean cut — no dual-mode period, no migration shim.
  // BBolt state was empty at time of migration; no in-flight checkpoints existed.
  // reserved 7;
  
  // NEW: Global Workspace — activation-threshold working set (all refs ≥ activation_threshold)
  // Each ContextRef is self-contained: activation + precision + snippet.
  // No separate precision map — it lived here in earlier drafts, now folded into ContextRef.
  repeated ContextRef working_memory = 8;
}

// BufferType enum removed (Q8 decision). Modality is implicit in Payload.type
// and ContextRef.labels — a separate routing enum added no machine-actionable
// information beyond what the capability name already encodes.
```

**Capacity limit:** `working_memory` contains all `ContextRef` items whose spreading activation energy meets or exceeds the `activation_threshold` config floor (default `0.3`). A hard safety ceiling of `max_context_slots` (default `20`) prevents graph explosion on dense graphs; when hit the executor logs `workspace_capacity_truncated=true`. There is no fixed integer cap — a step with a high-connectivity query legitimately receives more items than a low-connectivity one. Token budget enforcement lives downstream in the SDK `assemble_context()` helper, which truncates to the agent's actual prompt budget. The 7±2 / deadline-derived integer formulas are retired.

**Why this replaces 7±2:** Miller's Law applies to human digit spans and word lists. An LLM's "working memory" is its context window. A qwen3:8b agent with 6,000 usable tokens can reasonably hold ~30 chunks of 200 tokens each. Capping at 7 would artificially discard genuinely useful context. The capacity is a **resource-derived limit**, not a biological analogy. If spreading activation produces more candidates than `capacity`, the executor truncates and logs `workspace_capacity_truncated=true`.

### 2.6 New Configuration Fields

These fields appear in prose throughout this ADR but are never listed as concrete additions. They must be added to `configs/config.json` and `internal/config/config.go`:

```go
type ExecutionConfig struct {
    // ... existing fields ...

    // ADR-0022: Global Workspace capacity model
    //
    // ActivationThreshold is the post-BFS selection floor — NOT the same as
    // RetrievalFloor. RetrievalFloor is a cosine similarity floor for pgvector
    // ANN (should stay ~0.3). ActivationThreshold applies after SpreadingEngine.Spread()
    // to filter the returned GraphNodeExpansion list. BFS 1-hop nodes from a 0.75-score
    // seed carry ~0.15–0.25 activation energy after decay × edge_weight. Setting this
    // to 0.3 silently discards ALL multi-hop BFS results. Default 0.1 preserves 1-hop
    // neighbors. Tune downward (0.05) for graphs with sparse edges or weak activation_strength.
    ActivationThreshold    float64 `json:"activation_threshold"`      // default 0.1
    MaxContextSlots        int     `json:"max_context_slots"`         // default 20 — hard ceiling on working_memory length
    // ContextRefSnippetChars: minimum 500 chars (≈2–3 sentences) to be useful in
    // degraded mode. 150 chars is a 2-sentence fragment that can actively mislead
    // an LLM by anchoring a hallucination on a partial context. Binary payloads
    // (detected via utf8.Valid) receive an empty snippet rather than a garbled prefix.
    ContextRefSnippetChars int     `json:"context_ref_snippet_chars"` // default 500
}
```

Config defaults:
```json
{
  "activation_threshold": 0.1,
  "max_context_slots": 20,
  "context_ref_snippet_chars": 500
}
```

**`activation_threshold` vs `RetrievalFloor`:** These are two separate parameters controlling two different floors in the same pipeline:

| Config field | Applied at | Scale | Purpose |
|---|---|---|---|
| `retrieval_floor` (existing) | `ws.Store.Search` | cosine similarity [0,1] | Exclude semantically unrelated LTM documents from pgvector ANN |
| `activation_threshold` (new) | `PrimeForStep` post-BFS | activation energy [0,1] | Exclude structurally weak nodes from the working set |

Typical values differ by ~3×: `retrieval_floor=0.3`, `activation_threshold=0.1`. Confusing them (using RetrievalFloor as the activation threshold) kills all BFS-discovered nodes.

These are added in Phase 1 (used by `mergeStepResult` for snippet truncation) and Phase 2 (used by `PrimeForStep` for capacity control). Phase 0 requires no config changes.

### 2.7 What the Agent Receives

```python
# OLD (monolithic dump)
request.context = {
    "step_0_result": "500 tokens of REST...",
    "step_1_result": "500 tokens of GraphQL...",
    "ltm_doc_1": "300 tokens of API design...",
    # ... etc
}

# NEW (Global Workspace)
request.working_memory = [
    ContextRef(cid="QmA7f3...", type="ltm_doc", labels=["rest", "api"], activation=0.92),
    ContextRef(cid="QmB2e9...", type="step_result", labels=["step_0"], activation=0.45),
    ContextRef(cid="QmC4d1...", type="step_result", labels=["step_1"], activation=0.88),
]

```
# precision map removed from Handoff — folded into ContextRef field 6 (Q7)
# active_buffer / BufferType removed from Handoff entirely (Q8)

**Agent usage:**
```python
import cambrian_agent_sdk

@agent.capability("text_generation")
def handle(request):
    # assemble_context: free function, injectable fetch_fn for testability.
    # Sorts by activation×precision descending. Uses snippet if precision < fetch_threshold
    # or fetch_fn is None. Skips if precision < min_precision. Returns "" when empty.
    context_str = cambrian_agent_sdk.assemble_context(
        refs=request.working_memory,
        min_precision=0.5,
        max_tokens=800,
        fetch_fn=agent.substrate.get_context_node,  # omit → snippet-only degraded mode
        fetch_threshold=0.7,
    )
    prompt = f"{context_str}\n\n{request.payload.text}" if context_str else request.payload.text
    return agent.substrate.generate(
        request.session_token_id, prompt,
        timeout_ms=request.deadline_remaining_ms,
    )
```

**What `assemble_context` does internally:**
```python
def assemble_context(refs, min_precision=0.5, max_tokens=800,
                     fetch_fn=None, fetch_threshold=0.7):
    parts, tokens_used = [], 0
    # Sort by combined score: activation × precision (structural × semantic relevance)
    ranked = sorted(refs, key=lambda r: r.activation * r.precision, reverse=True)
    for ref in ranked:
        if ref.precision < min_precision:
            continue
        if fetch_fn and ref.precision >= fetch_threshold:
            node = fetch_fn(ref.cid)
            text = node.data.decode("utf-8") if node else ref.snippet
        else:
            text = ref.snippet  # ContentStore not called — degraded but functional
        chunk = text[: (max_tokens - tokens_used) * 4]  # rough chars-per-token
        parts.append(f"[{ref.type}] {chunk}")
        tokens_used += len(chunk) // 4
        if tokens_used >= max_tokens:
            break
    return "\n".join(parts)
```

**Why this matters:** The original draft required every agent author to write 10+ lines of dereference/filter/truncate/format logic. That pattern will be copy-pasted with bugs (wrong threshold, wrong truncation, wrong ordering). `assemble_context()` encapsulates the boilerplate in the SDK, giving agent authors a single correct call. The injectable `fetch_fn` makes it testable without gRPC infrastructure.

---

## 3. The `mergeStepResult` Refactor

Currently:
```go
func mergeStepResult(masterContext map[string]string, r stepResult) {
    key := fmt.Sprintf("step_%d_result", r.index)
    masterContext[key] = string(r.resp.Payload.Data)
}
```

New version:
```go
func mergeStepResult(masterRefs []ContextRef, r stepResult, store ContentStore) ([]ContextRef, error) {
    data := r.resp.Payload.Data

    // Deduplicated write: Put is a no-op if SHA-256(data) already exists.
    cid, err := store.Put(data, "step_result", []string{fmt.Sprintf("step_%d", r.index), "result"})
    if err != nil {
        return nil, err
    }

    // Snippet: first snippetBytes chars — resilience primitive for ContentStore hiccups.
    snippet := utf8Truncate(string(data), snippetBytes)

    ref := ContextRef{
        CID:     cid,
        Type:    "step_result",
        Labels:  []string{fmt.Sprintf("step_%d", r.index), "result"},
        Snippet: snippet,
        // Activation and Precision are set by PrimeForStep() at dispatch time,
        // not here at write time — they are query-relative, not content-absolute.
    }

    // Agent-added step_N_{k} metadata keys are intentionally NOT stored as ContextRefs.
    // They were additive annotations from prior agents; injecting them into downstream
    // working sets caused context pollution (Q3 decision).

    return append(masterRefs, ref), nil
}
```

**Key change:** `masterContext` is no longer `map[string]string`. It is `[]ContextRef` — lightweight handles. Content lives in the ContentStore; `activation` and `precision` are populated at dispatch time by `PrimeForStep()`, not at write time.

**GC must be deferred, not called inline:**
```go
func (e *DAGExecutor) Execute(ctx context.Context, plan domain.Plan) error {
    var planCIDs []CID
    defer func() {
        // Deferred: runs even on panic, preventing CID accumulation.
        // Synchronous GC on a busy bbolt serializes with concurrent plan completions
        // — consider an async GC goroutine if concurrent plan completion rate is high.
        if err := e.ContentStore.GC(planCIDs); err != nil {
            slog.Warn("ContentStore GC failed", "err", err)
        }
    }()
    // ... plan execution ...
}
```

Without `defer`, a panic in `Execute` skips GC entirely and ContentStore accumulates indefinitely. The async GC pattern (a separate goroutine coalescing CID sets from completed plans) is preferable under high concurrent plan throughput — bbolt's write lock serializes all GC calls, so simultaneous plan completions create a latency cliff at the start of the next plan waiting for the GC transaction to complete.

---

## 4. Deferred to ADR-0023

Interleaved replay (CLS integration) and session-scoped consolidation were originally in this section. Both depend on session-scoped GC (deferred, see `docs/future_work_summary.md`) and a `MemoryConsolidator` goroutine separate from the existing `MemoryWorker` and `ConsolidatorAgent`. Scope was stripped to avoid three concurrent consolidation paths before any are validated.

See `docs/future_work_summary.md` → **ADR-0023 (Consolidation & Interleaved Replay)** for the full deferred design.

---

## 5. Flaws, Risks & Honest Assessment

### ⚠️ FLAW 1: High Latency on Every Step Dispatch (Mitigated by Phasing + Caching)

`PrimeForStep` is called **once per step**, immediately before dispatch. Correct per-operation breakdown (nomic-embed-text via Ollama on local GPU):

| Operation | Typical Latency | Why |
|-----------|----------------|-----|
| `ws.Embedder.Embed(query)` | 3–20ms | Forward pass through a 137M-param transformer — no autoregressive sampling, no KV-cache. Short query (<100 tokens): ~3–8ms; medium (100–500 tokens): ~10–20ms. |
| `ws.Store.Search(ctx, vec, {TopK:20})` | 10–30ms | pgvector HNSW ANN index scan. Scales with `ef_search` and table cardinality. Main variable as LTM grows. |
| `ws.SpreadingEngine.Spread(ctx, seeds)` | 10–40ms | BFS over `document_edges` (PostgreSQL). Scales with graph degree and `MaxDepth`. Nil-safe: skips if graph unpopulated. |
| `ws.ContentStore.Get()` per snippet | 1–5ms each | bbolt bucket lookup. 5–10 fetches = 5–50ms total. LRU-cached after first access. |

**Corrected total: ~28–140ms per step.** For a 10-step plan: **~280ms – 1.4s on the critical path.** The 50–100ms embedding figure from the original draft was LLM *generation* latency (autoregressive sampling) mistakenly applied to embedding inference — these are different operations.

**Real bottleneck: database, not embedder.** The embedder is fast (3–20ms). The variable-cost operations are the pgvector ANN query and the BFS `document_edges` traversal, both of which hit PostgreSQL. Under concurrent plan execution, the bottleneck is PostgreSQL connection saturation, not `OllamaEmbedder`. Monitor `pg_stat_activity` queue depth, not embedder throughput.

**Mitigation:** `WorkspaceStageImpl` maintains a bounded LRU cache `query → []ContextRef` (max 100 entries) — skips both embedder and DB round-trips for repeated queries. **Invalidation contract: TTL alone is insufficient.** The Tier-2 drain (`MemoryAgent.drainBatch`) commits new documents to pgvector when the Tier-1 buffer fills (default batch size 32) OR when the idle timer fires. Under active plan execution this happens within minutes. A 5-minute TTL can serve stale ContextRefs that exclude recently-committed documents. The cache must be invalidated on every `commitBatch` event — wire a `CacheInvalidator` callback. Separately, a `query → embedding` cache (TTL-free, embeddings are deterministic) reduces the embedder round-trip on the remaining cache misses.

**Status: MITIGATED.** This latency only materializes in **Phase 2** (after ADR-0016/0017 graph population). Phase 0 has **zero latency cost** — `step.DependsOn` is an in-memory index lookup. Phase 1 adds ~5ms per bbolt `Get()`. The full 28–140ms cost is deferred. By placing spreading activation in `WorkspaceStage` (not `DAGExecutor`), the database overhead is isolated — degradation in PrimeForStep does not affect the dispatch loop itself.

### ⚠️ FLAW 2: The Graph Is Empty (Cold Start)

Spreading activation requires edges. ADR-0017 (`document_edges`) is not yet populated. Until agents write edges, graph propagation is a no-op.

**Status: MITIGATED.** Phase 2 explicitly falls back to flat similarity search when `graph_degree < threshold`. Phase 0 and Phase 1 work without any graph. We only enable graph propagation once the graph has meaningful connectivity.

### ⚠️ FLAW 3: "Merkle DAG" Was the Wrong Terminology

The original draft described Layer 2 as a "Merkle DAG." A true Merkle DAG computes parent CIDs from child CIDs, creating cascade invalidation on replanning. This would break the executor's `ReplanHandler` — every ancestor CID would change when a child step is rewritten.

**Status: FIXED.** Layer 2 is now correctly described as a **Content-Addressed Store (CAS)**. `CID = SHA-256(data)` independently of parent relationships. Provenance edges are metadata, not part of the hash. No cascade invalidation. Deduplication still works. Auditability is preserved via the separate provenance table.

### ⚠️ FLAW 4: Proto Breaking Change

`Handoff` is the core proto message. Removing `context = 7` would break every test and agent.

**Status: RESOLVED — CLEAN CUT.** BBolt state was empty at migration time; no in-flight checkpoints. `context = 7` is removed immediately when Phase 3 ships. No dual-mode period, no migration shim. Field number 7 is reserved. All tests are updated at the same commit as the proto change. The Python SDK `server.py` reads `working_memory` only.

### ⚠️ FLAW 5: Precision Weights Are a Heuristic

"Precision = embedding similarity" is a simplification of Friston's Predictive Coding. True FEP precision requires a full generative model of the agent's beliefs.

**Status: ACCEPTED RISK.** Precision weights are **hints**, not commands. `precision` is a field on each `ContextRef` (not a separate Handoff map). `assemble_context()` uses it as a sort key; the agent author controls `min_precision` and `fetch_threshold`. If the weights are wrong, the agent filters them by adjusting thresholds. Log `workspace_capacity_truncated` and `precision_skip_count` to measure calibration drift.

### ⚠️ FLAW 6: Capacity Limit Was Arbitrary

The original draft used "7±2 items" (Miller's Law). An LLM's context window is 8K-128K tokens — it can handle 30+ chunks of 200 tokens each. Capping at 7 would artificially discard useful context.

**Status: FIXED.** Capacity is **activation-threshold-driven**: all items with `activation ≥ activation_threshold` (default `0.3`) are included, up to a hard ceiling of `max_context_slots` (default `20`). There is no fixed integer cap. A high-connectivity query naturally receives more items; a sparse-graph query receives fewer. Token budget enforcement lives downstream in `assemble_context()`. The 7±2 and deadline-derived formulas are both retired.

### ⚠️ FLAW 7: Hexagonal Violation — Spreading Activation in DAGExecutor

The original draft placed `igniteWorkingSet` (graph BFS + embedding + precision weighting) inside `DAGExecutor`. This breaks the hexagonal boundary — the executor should not touch pgvector or `document_edges`.

**Status: FIXED.** Spreading activation and precision weighting now live in `WorkspaceStage.PrimeForStep()`. The executor calls `workspace.PrimeForStep(ctx, query, priorCIDs, maxItems)` and receives a ranked `[]ContextRef`. The executor does not know how the list was assembled. This restores the separation and means the retrieval algorithm can be swapped without touching the executor.

### ⚠️ FLAW 8: Agent API Was Too Complex

The original draft required every agent author to write 10+ lines of CID dereference, precision filtering, truncation, and formatting. This boilerplate would be copy-pasted with bugs.

**Status: FIXED.** The Python SDK provides `cambrian_agent_sdk.assemble_context(refs, min_precision, max_tokens, fetch_fn, fetch_threshold)` — a free function with injectable `fetch_fn` for testability. Snippet-only degraded mode when `fetch_fn=None`. Agent authors write one line:
```python
context_str = cambrian_agent_sdk.assemble_context(refs=request.working_memory, min_precision=0.5, max_tokens=800, fetch_fn=agent.substrate.get_context_node)
```
The "developer sees neither" principle. `request.assemble_context()` as a method was considered and rejected — it would require circular imports between `types.py` and `__init__.py`.

### ⚠️ FLAW 9: We Don't Know If It Works

The token savings are theoretical. The latency costs are real. We have no benchmark measuring plan quality with vs. without Global Workspace.

**Status: MITIGATED.** Phase 0 (executor filtering) validates the core hypothesis **immediately** — no new infrastructure, no latency cost. If plan quality drops from 4.2 to <4.0 with filtered context, we learn that before spending weeks on CAS and proto changes. ADR-0021's E2E benchmark is extended with the suite defined in Section 8.2: `BenchmarkE2E_Baseline` establishes actual baselines before any phase ships, then `BenchmarkE2E_Phase0_TokenReduction` gates Phase 0 → Phase 1, and `BenchmarkE2E_Phase3_GlobalWorkspace` gates Phase 3 → Production.

### ⚠️ FLAW 10: ContentStore Becomes SPOF

If the ContentStore (bbolt) becomes corrupt:
- Agents can't dereference CIDs
- Executor can't verify prior steps

**Status: MITIGATED WITH CAVEATS.** Each `ContextRef` carries an inline `snippet`. If `get_context_node(cid)` fails, `assemble_context()` falls back to snippet-only mode — degraded but functional. Two constraints:

1. **Snippet length floor:** `context_ref_snippet_chars` default raised from 150 → **500**. A 150-char fragment of a 2,000-char step result is 7.5% of content — a truncated analysis can actively mislead an LLM by anchoring hallucination on a fragment. 500 chars (≈3–5 sentences) gives enough context to be useful. If `len(data) ≤ context_ref_snippet_chars`, the full content is inlined — no truncation.

2. **Binary payload guard:** `Put()` must detect non-UTF-8 content via `utf8.Valid(data)`. Binary payloads (image bytes, protobuf binary, structured JSON with escape sequences) must receive an empty snippet rather than a `utf8Truncate` that returns a garbled prefix. A garbled 500-byte prefix is worse than no snippet at all.

ContentStore is an optimisation layer. The snippet is the resilience primitive, but only when it carries meaningful text content.

---

## 6. Sequenced Implementation Plan

### Phase 0: DAG-Declared Context Filtering (Now — No New Infrastructure)
**Goal:** Eliminate O(N²) token growth using `step.DependsOn` — no regex, no heuristics.

- [ ] Add `filterSnapshotForStep(master map[string]string, step domain.Step) map[string]string` to `dag_executor.go`
- [ ] Filter rules (Q3 decision):
  - Include `step_N_result` **only** for N ∈ `step.DependsOn`
  - Strip all `step_N_{k}` agent-metadata keys entirely
  - Preserve all non-step keys (`ltm_*`, user `initialContext`, substrate metadata)
- [ ] Call `filterSnapshotForStep` when building the `Handoff` for each step dispatch
- [ ] Log `executor_context_filtered=true, depend_keys=N, total_prior_keys=M`

**Zero proto changes. Zero new storage. Zero SDK changes.** If a step needs a prior result, the planner must declare it in `DependsOn` — the executor does not paper over planner omissions with language heuristics. Note: `needs_prior_context()` was built and then removed — it was the wrong abstraction at the wrong layer (Q13/Q14 decisions).

**Test impact:** `dag_executor_test.go` tests that assert "step N receives all prior `step_M_result` keys" must be narrowed to "step N receives only `step_M_result` for M ∈ `step.DependsOn`." Tests asserting that `step_N_{k}` agent-metadata keys are propagated must be updated to assert they are stripped. These test changes must land in the same commit as `filterSnapshotForStep`.

### Phase 1: Content-Addressed Store (After ADR-0015 — Schema Migration Complete)
**Goal:** Deduplication and cross-plan sharing without touching the proto contract.

- [ ] `ContentStore` interface + `BBoltContentStore` implementation
- [ ] Refactor `mergeStepResult` to store step results in CAS, write CID into `masterContext["step_N_cid"]`
- [ ] `masterContext` still carries strings for backward compat; new `"_cid"` suffixed keys are additive
- [ ] `internal/storage/content_store_test.go`

**Proto untouched. Python SDK untouched. Executor loop touched only in `mergeStepResult`.**

### Phase 2: WorkspaceStage Selection (After **Phase 1** AND ADR-0016/0017 — Graph Populated)
**Goal:** Structured retrieval via spreading activation, without proto changes.

**Prerequisite: Phase 1 must be complete.** `PrimeForStep` receives `priorStepRefs []ContextRef` with real CIDs. Those CIDs only exist after Phase 1 writes step results to CAS. Attempting Phase 2 before Phase 1 means `priorStepRefs` is always empty — the DependsOn boost has nothing to boost.

- [ ] Add `ContentStore` and `ActivationThreshold float64` fields to `WorkspaceStageImpl` (both injected at wire time)
- [ ] Add bounded LRU cache `query → []ContextRef` (max 100 entries) — invalidated on every Tier-2 pgvector drain event (not TTL-only; see cache invalidation note below)
- [ ] Extend `WorkspaceStage` interface: `PrimeForStep(ctx, query, priorStepRefs []ContextRef, maxItems int) ([]ContextRef, error)`
- [ ] Implement `PrimeForStep` using `ws.Store.Search` → seeds, `ws.SpreadingEngine.Spread` → expansions, activation threshold = `ws.ActivationThreshold` (NOT `ws.RetrievalFloor`)
- [ ] `DAGExecutor` calls `workspace.PrimeForStep(ctx, step.Query, priorCIDs, maxContextSlots)` per step, immediately before dispatch
- [ ] Injection (dry-run): JSON-serialize the returned `[]ContextRef` into `masterContext["ltm_prime_refs"]` (single string key)
- [ ] **Validation mechanism** (this is the whole point of Phase 2): log `prime_for_step_selected=[cid1,cid2,...], activations=[0.82,0.19,...], step=N, plan=X` for every `PrimeForStep` call. Add `BenchmarkPhase2Selection` that runs 10 known plans, collects selection logs, and checks that (a) DependsOn-declared step CIDs appear in the selected set with boosted activation, (b) `workspace_capacity_truncated` does not fire on typical plans. Without this, the Phase 2 → Phase 3 gate is blind.
- [ ] Run the activation_threshold sensitivity analysis: measure `avg_context_slots` at threshold ∈ {0.05, 0.08, 0.10, 0.15, 0.20} against the benchmark corpus. Document the inflection point before choosing Phase 3 defaults.
- [ ] Monitor PostgreSQL connection queue depth (`pg_stat_activity`) under concurrent plans; log `embedder_cache_hit=true` when LRU serves a query

**Cache invalidation contract:** The `query → []ContextRef` cache must be invalidated on every Tier-2 pgvector drain (`MemoryAgent.drainBatch`). Tier-2 drains when `pendingItems >= tier2BatchSize` (default 32) OR `tier2MaxIdle` fires. Under active plan execution, drains happen within minutes of plan completion — far shorter than any TTL. A 5-minute TTL alone can serve stale results that exclude recently-committed step results. Wire a `CacheInvalidator` callback into `MemoryAgent.commitBatch` to notify `WorkspaceStageImpl`.

**Proto untouched. Phase 2 is a validation phase — it must produce observable evidence that selection is correct before Phase 3 commits the proto change.**

### Phase 3: Proto Contract & Global Workspace (After Phase 2 Validation)
**Goal:** Replace `map<string,string>` with `[]ContextRef` — clean cut, no dual-mode.

- [ ] Proto changes: `ContextRef` message (cid, type, labels, activation, precision, snippet); `working_memory = 8` on Handoff; `reserved 7` (context map removed)
- [ ] `BufferType` NOT added — removed during design (Q8)
- [ ] No separate `precision` map on Handoff — folded into `ContextRef.precision` (Q7)
- [ ] Python SDK: `assemble_context()` free function with injectable `fetch_fn` (Q10)
- [ ] Python SDK: `SubstrateClient.get_context_node(cid)` for full-node dereference
- [ ] All tests updated at same commit as proto change (no migration period — BBolt empty)
- [ ] E2E benchmark: measure token reduction, latency impact, plan quality score
- [ ] If quality drops below 4.0 → revert Phase 3, investigate Phase 2 calibration first

### Phase 4: Consolidation & Interleaved Replay (ADR-0023)
**Goal:** Episodic traces → semantic LTM consolidation.

- [ ] `MemoryConsolidator` goroutine (nightly, triggered by `CircadianRhythm`)
- [ ] Interleaved replay: recent traces + historical samples → pgvector re-embedding
- [ ] Metrics: `consolidation_traces_processed`, `semantic_docs_updated`

**Deserves its own ADR. See ADR-0023 (draft).**

---

## 7. Success Criteria

| Metric | Current (map<string,string>) | Target (Global Workspace) | Measurement |
|--------|------------------------------|---------------------------|-------------|
| Tokens injected per step (3-step plan avg) | TBD — establish via `BenchmarkE2E_Baseline` | <600 | `executor_context_filter` event → `keys_passed` aggregation |
| Tokens injected per step (10-step plan avg) | TBD — establish via `BenchmarkE2E_Baseline` | <1,200 | Same |
| Plan quality score (LLM-as-Judge 1-5) | TBD — establish via `BenchmarkE2E_Baseline` | ≥baseline−0.2 (no regression) | `BenchmarkE2E_Phase3_GlobalWorkspace` |
| Step dispatch latency (Phase 0/1) | ~5ms | <20ms | `execute_duration_ms` event |
| Step dispatch latency (Phase 2, with PrimeForStep) | ~5ms | <150ms | `workspace_prime_for_step duration_ms` event |
| BFS contribution (bfs_fraction) | 0% (no graph) | >10% after graph populated | `workspace_prime_for_step bfs_fraction` event |
| Cross-plan memory recall (ltm_hit_rate) | 0% | >30% after Phase 2 | `workspace_prime_for_step ltm_hits` aggregation (see §8.1) |
| Test pass rate | 36/36 | 36/36 | `go test ./...` |

**Note:** "~1,500 tokens/step" and "4.2 quality" were theoretical estimates. `BenchmarkE2E_Baseline` must run before Phase 0 ships to establish empirical baselines. All subsequent targets are expressed as deltas from that baseline, not from theoretical estimates.

### Statistical Process Control

Targets without control limits are wishes, not process controls. The table below adds Upper Control Limits (UCL) and explicit reaction plans:

| Metric | Target | UCL (trigger reaction) | Reaction Plan | TTR |
|--------|--------|------------------------|---------------|-----|
| Tokens/step (3-step) | <600 | >800 for 2 consecutive plans | Log `context_filter_regression=true`; check `filterSnapshotForStep` — likely a DependsOn misconfiguration in the planner | <30 min |
| Tokens/step (10-step) | <1,200 | >2,000 for 2 consecutive plans | Same as above | <30 min |
| Plan quality (LLM-as-Judge) | ≥4.0 | <3.5 for any single plan, or <4.0 for 3 consecutive plans | Toggle `use_global_workspace: false` (feature flag — seconds to revert); investigate Phase 2 calibration before re-enabling | <5 min via flag; 4–8h via proto revert |
| Step dispatch latency (Phase 2) | <150ms | >250ms for 3 consecutive steps | Degrade to snippet-only mode (set `fetch_fn=None` in `assemble_context`); log `latency_degrade=true`; check `pg_stat_activity` — bottleneck is PostgreSQL, not embedder | <5 min |
| ContextRef LRU cache hit rate | >40% | <20% sustained | Review cache invalidation wiring; check if `CacheInvalidator` callback is firing on `commitBatch`; check for query diversity explosion | <1h |

### Measurement System Analysis (Required Before Phase 2 SPC)

The quality target "≥4.0" is currently unfalsifiable because the measurement system (LLM-as-Judge) has not been characterized. Before using judge scores for SPC, run an MSA:

1. **Repeatability:** Run the same 10 benchmark plans through `BenchmarkE2E_PlanQualityJudge` five times each (same prompts, same outputs, different judge invocations). Compute σ (standard deviation of scores for each plan). If σ > 0.3, the judge has high measurement noise — the UCL of 3.5 falls inside the noise floor and will generate constant false alarms.
2. **Judge model:** The judge must NOT be the same model as the executing agent (qwen3:8b). A model grading its own outputs exhibits systematic self-preference bias. Use a separate model (e.g., a larger instruction-tuned model) as judge.
3. **Calibration anchors:** The judge prompt must include scored examples: "A score of 1 means completely wrong, 3 means partially correct, 5 means fully correct with no errors." Without anchors, the absolute scale drifts across API calls.

If MSA reveals σ > 0.3, the quality SPC chart cannot be used for Phase Gate decisions. In that case, use token reduction (objective, low-variance) as the primary gate metric, and treat quality scores as supplementary signal only.

### `activation_threshold` Sensitivity Analysis (Required Before Phase 2 Rollout)

`activation_threshold` is a **step-function control parameter**, not a linear dial. In a graph with power-law degree distribution (which `document_edges` will develop), small threshold changes produce large context-window changes:

- threshold=0.10 → ~8–15 items (1-hop BFS included)
- threshold=0.15 → ~4–8 items (only high-energy 1-hop included)
- threshold=0.20 → ~2–4 items (mostly seeds only)
- threshold=0.30 → 0–2 items (BFS effectively disabled — seeds only)

Before Phase 2 rollout, run `BenchmarkPhase2Selection` at threshold ∈ {0.05, 0.08, 0.10, 0.15, 0.20} and plot `avg_context_slots` vs threshold. The inflection point (where context_slots stabilizes) is the stable operating region. Choose the default from within that region, not from first principles. Document the graph's actual activation energy distribution as part of the Phase 2 → Phase 3 gate criteria.

### Feature Flag for Phase 3 Revert

**Do not rely solely on proto revert as the reaction plan.** A proto revert touches the wire format, Go generated structs, Python SDK types, and all test fixtures — a realistic 4–8h operation. Add a feature flag `use_global_workspace: bool` (default `true` after Phase 3) to `ExecutionConfig`:

- `true`: executor populates `Handoff.working_memory` from `[]ContextRef`
- `false`: executor falls back to Phase 0 behavior — `filterSnapshotForStep` into `Handoff.context`

The proto carries both fields; only one is populated at runtime depending on the flag. This is not "dual-mode" (both never active simultaneously) — it's a **circuit breaker**. Flipping the flag restores Phase 0 behavior in seconds without a proto revert. The clean-cut proto migration (removing field 7) still happens; the flag controls which field is written, not whether both are maintained indefinitely.

**Revert policy:** quality UCL breach → flip flag first (TTR <5 min); investigate calibration; if calibration fix takes >24h, open a proto revert PR in parallel.

---

## 8. Observability & Benchmark Suite

### 8.1 Structured Telemetry Events

All events use `log/slog` key-value format (existing convention). Each event must include `plan_id` and `step_index` for trace correlation across the pipeline. Events without these fields cannot be correlated to specific plan executions in `tools/export-events`.

#### Phase 0 — Executor context filter

```go
slog.Info("executor_context_filter",
    "plan_id",       planID,
    "step_index",    stepIdx,
    "depend_count",  len(step.DependsOn),   // DependsOn-declared dependencies
    "keys_passed",   keysPassedCount,        // step_N_result keys that survived
    "keys_stripped", keysStrippedCount,      // step_N_{k} metadata keys removed
    "prior_total",   totalPriorKeys,         // keys in master before filter
)
```

#### Phase 1 — Content-Addressed Store

```go
// On every Put() call
slog.Info("content_store_put",
    "plan_id",    planID,
    "step_index", stepIdx,
    "cid",        cid,
    "dedup",      wasExisting,   // true = SHA-256 matched existing node; no write occurred
    "data_bytes", len(data),
    "has_snippet", snippet != "",
)

// On every GC call (use defer in Execute() — must fire even on panic)
slog.Info("content_store_gc",
    "plan_id",      planID,
    "kept_cids",    len(keepCIDs),
    "evicted_cids", evictedCount,
    "duration_ms",  gcDurationMs,
)
```

**Derived metric:** `dedup_rate = Put() calls where dedup=true / total Put() calls`. Low early on (all unique content); rises as step results converge across plans. Track via `tools/export-events` aggregation.

#### Phase 2 — PrimeForStep selection

```go
slog.Info("workspace_prime_for_step",
    "plan_id",            planID,
    "step_index",         stepIdx,
    "seeds_returned",     len(seeds),        // pgvector ANN hits before BFS
    "expansions_total",   len(expansions),   // SpreadingEngine.Spread() output
    "selected_count",     len(items),        // after activation_threshold filter
    "truncated",          truncated,         // hit max_context_slots ceiling
    "bfs_fraction",       bfsFraction,       // fraction where precision==-1.0 (BFS-discovered)
    "activation_p50",     p50Activation,     // median activation in selected set
    "activation_min",     minActivation,     // lowest activation that passed threshold
    "cache_hit",          cacheHit,          // LRU served this query
    "duration_ms",        durationMs,
)

// Only when truncated=true — gives calibration signal for max_context_slots
slog.Warn("workspace_capacity_truncated",
    "plan_id",               planID,
    "step_index",            stepIdx,
    "kept",                  maxItems,
    "total_before_cut",      totalBeforeCut,
    "activation_min_kept",   items[maxItems-1].Activation,
    "activation_max_dropped", allItems[maxItems].Activation,
)
```

**Key ratio:** `bfs_fraction` directly measures whether spreading activation is doing useful work. If `bfs_fraction` is consistently 0, the graph is empty or the activation_threshold is too high — BFS is contributing nothing. This metric is the Phase 2 gate signal.

#### Phase 3 — SDK `assemble_context()` (Python, stdout JSON)

```python
logger.info("assemble_context_done",
    extra={
        "cid_count":           len(refs),
        "tokens_used":         tokens_used,
        "refs_skipped":        skipped_precision,  # precision < min_precision
        "refs_fetched_full":   fetched_full,        # ContentStore fetch succeeded
        "refs_used_snippet":   used_snippet,        # snippet fallback
        "degraded_mode":       fetch_fn is None,
        "plan_id":             request.plan_id,     # requires plan_id on ExecuteRequest
        "step_index":          request.step_index,
    }
)
```

**Observability boundary:** `assemble_context()` runs inside the Python agent process. Its telemetry is emitted to agent stdout (SlogHandler JSON). The Go substrate never sees these events unless it reads the agent's stdout pipe. This is a split observability model — `workspace_prime_for_step` events (Go side) and `assemble_context_done` events (Python side) must be correlated via `plan_id + step_index`. Operators monitoring only Go substrate logs have a blind spot over the SDK half of the context pipeline.

#### `ltm_hit_rate` — Defined

`ltm_hit_rate` was referenced throughout this ADR but never defined. Formal definition:

> An **LTM hit** occurs when a step's `working_memory` contains at least one `ContextRef` where `type = "ltm_doc"` AND whose `cid` was written to pgvector during a **prior plan** (not the current plan's step results).
>
> `ltm_hit_rate = (steps with ≥1 LTM hit) / (total steps across all plans in the benchmark window)`

This measures cross-plan semantic retrieval — the core value proposition of Phase 2. Track in `workspace_prime_for_step` by adding `ltm_hits: int` (count of selected items with type="ltm_doc" and plan_provenance != current_plan_id).

### 8.2 Benchmark Suite

#### Microbenchmarks (`//go:build bench`)

These run in isolation with no real gRPC, pgvector, or Ollama. Each file lives in the same package as the unit being tested.

| Benchmark | File | What It Measures |
|-----------|------|------------------|
| `BenchmarkFilterSnapshotForStep_3deps` | `dag_executor_bench_test.go` | Phase 0 filter latency, 3-step DependsOn |
| `BenchmarkFilterSnapshotForStep_10deps` | Same | Phase 0 filter latency, 10-step DependsOn |
| `BenchmarkContentStorePut_Miss` | `content_store_bench_test.go` | CAS write, new CID (bbolt write tx) |
| `BenchmarkContentStorePut_Hit` | Same | CAS write, existing CID (bbolt read + early return) |
| `BenchmarkContentStorePut_Concurrent` | Same | 4 goroutines writing, measures bbolt write lock contention |
| `BenchmarkContentStoreGC_100CIDs` | Same | GC with 100 CIDs — expected <5ms |
| `BenchmarkContentStoreGC_Concurrent` | Same | 4 GC calls simultaneously — measures bbolt lock serialization lag |
| `BenchmarkPrimeForStep_EmptyGraph` | `workspace_stage_bench_test.go` | Phase 2, SpreadingEngine nil — seed-only path |
| `BenchmarkPrimeForStep_SyntheticGraph_N50` | Same | Phase 2 with 50-document graph, depth=2 |
| `BenchmarkPrimeForStep_SyntheticGraph_N500` | Same | Phase 2 with 500-document graph — models production graph size |
| `BenchmarkAssembleContext_10refs` | `sdk_bench_test.py` | Python SDK `assemble_context()` with 10 refs, fetch_fn stub |
| `BenchmarkAssembleContext_DegradedMode` | Same | Snippet-only path, no fetch_fn |

**Pass criteria:** All microbenchmarks must show:
- `BenchmarkFilterSnapshotForStep_*` < 1ms (it's a map iteration)
- `BenchmarkContentStorePut_*` < 10ms single-threaded
- `BenchmarkContentStoreGC_Concurrent` — `max_latency - min_latency < 50ms` (bounds lock contention)
- `BenchmarkPrimeForStep_EmptyGraph` < 20ms (seed-only path)
- `BenchmarkPrimeForStep_SyntheticGraph_N500` < 150ms (within latency target)

#### E2E Benchmarks (`//go:build e2e`)

These require a live Postgres + pgvector + Ollama (nomic-embed-text). They are not run in CI but are run manually before each phase gate and results are committed to `docs/benchmarks/`.

| Benchmark | Phase gate | What It Measures |
|-----------|-----------|------------------|
| `BenchmarkE2E_Baseline` | Before Phase 0 | **Establish actual baselines**: token counts per step, quality scores, dispatch latency. Required before any phase ships — the "4.2 quality" and "1,500 tokens/step" targets are currently theoretical. This benchmark makes them empirical. |
| `BenchmarkE2E_Phase0_TokenReduction` | Phase 0 → Phase 1 gate | Token count before/after `filterSnapshotForStep` on 3-step and 10-step plans. Must show ≥50% reduction for 10-step plans to confirm O(N²) is fixed. |
| `BenchmarkE2E_Phase1_DedupRate` | Phase 1 → Phase 2 gate | Run same plan corpus twice; measure `dedup_rate` from `content_store_put` logs. If `dedup_rate = 0%`, plans are producing all-unique content — deduplication provides no value yet. Document and proceed. |
| `BenchmarkPhase2Selection` | Phase 2 → Phase 3 gate | For 10 known plans: (a) assert DependsOn CIDs appear in selected set with boosted activation; (b) assert `bfs_fraction > 0%` (spreading activation is contributing nodes); (c) run threshold sweep ∈ {0.05,0.08,0.10,0.15,0.20} and record `avg_context_slots` per threshold. **Gate fails if `bfs_fraction = 0%` at default threshold — BFS is disabled, Phase 2 is just flat similarity search.** |
| `BenchmarkE2E_Phase3_GlobalWorkspace` | Phase 3 → Production gate | Token reduction + quality score + latency against baseline. Quality must be ≥4.0 (or ≥baseline−0.2 if MSA reveals baseline < 4.2). Latency must be <150ms/step. |
| `BenchmarkE2E_ConcurrentPlans_N4` | Phase 3 → Production gate | 4 concurrent plans, measure: (a) GC lock contention (`content_store_gc duration_ms` variance); (b) PostgreSQL queue depth at peak; (c) per-step latency p99 vs single-plan baseline. |

#### Regression Benchmark (CI, every PR)

```
//go:build regression

BenchmarkRegression_Phase0: runs BenchmarkE2E_Phase0_TokenReduction against
a pinned 3-step plan. Fails if tokens/step > UCL (800). Runs in <60s on
a dev machine with a local Postgres.
```

This is the only E2E test that runs in CI. It uses a pinned synthetic plan with a mock LLM (deterministic stub) so it doesn't require Ollama. It validates that `filterSnapshotForStep` hasn't regressed — not plan quality, just token counts.

### 8.3 Results Tracking

Benchmark results must be committed to `docs/benchmarks/` as Markdown tables with:
- Date
- Git SHA
- Phase active
- Hardware profile (CPU, RAM, GPU, Postgres version)

Format:
```
docs/benchmarks/
  phase0_YYYY-MM-DD_<sha>.md
  phase1_YYYY-MM-DD_<sha>.md
  phase2_YYYY-MM-DD_<sha>.md
  phase3_YYYY-MM-DD_<sha>.md
```

This creates an audit trail for phase gate decisions and a corpus for detecting performance regressions across refactors.

---

## 9. References

- **Baars, B. J. (1988).** *A Cognitive Theory of Consciousness*. (Global Workspace Theory)
- **Friston, K. (2010).** The free-energy principle: a unified brain theory? *Nature Reviews Neuroscience*. (Predictive Coding / FEP)
- **Baddeley, A. D., & Hitch, G. (1974).** Working memory. (Multi-component Working Memory)
- **McClelland, J. L., McNaughton, B. L., & O'Reilly, R. C. (1995).** Complementary Learning Systems. (Interleaved Replay)
- **Collins, A. M., & Loftus, E. F. (1975).** Spreading-activation theory of semantic processing. (Graph Propagation)
- ADR-0015: Engram Engine (LTM storage + retrieval)
- ADR-0016: Global Workspace Stage (enrichment layer)
- ADR-0017: Spreading Activation Engine (document graph)
- ADR-0021: System Quality Measurement (benchmarks + telemetry)
- `docs/theory/Free_Enerygy_Principle.md`
- `docs/theory/Global_Workspace_Theory.md`
- `docs/theory/memory/Baddeley_Hitch_Working_Memory_Model.md`
- `docs/theory/memory/Complementary_Learning_Systems.md`
- `docs/theory/memory/Spreading_Memory_Theory.md`
