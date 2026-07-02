# ADR-0016: Global Workspace Stage — Bounded Working Memory Architecture

**Status:** Implemented
**Date:** 2026-05-17
**Deciders:** Afsin, Claude

**Prerequisites:** ADR-0015 (Engram Engine — must be implemented first)

**Implementation Dependency Graph:**

Hard dependencies on ADR-0015:
- Tier-1 channel (merged cosine+channel retrieval path)
- `DocTypeMnemonicFact` / `DocTypeMnemonicScene` constants
- `activation_strength` column (re-ranking formula in base hit phase)

ADR-0016 unblocks ADR-0017:
- `WorkspaceStage` interface definition
- Enrichment map metadata format (Graph Coverage Guard tags, `[CONFLICT]` tags)

Parallelisable before ADR-0015 Tier-2 goroutine is live:
- `WorkspaceStage` struct definition
- `PrimeForPlanning` / `PrimeForExecution` stub interfaces
- Contradiction detection logic (pgvector cosine check requires only VectorStore, not Tier-2)

Blocked until ADR-0015:
- Tier-1 channel running (merged cosine+channel retrieval path)
- `activation_strength` column present in schema

---

## Context

ADR-0015 scoped the LTM layer only — pgvector schema, write pipeline, retrieval, and decay. It explicitly excluded the `masterContext map[string]string` replacement, noting it has "a completely different blast radius — it touches `DAGExecutor` directly" and warrants its own ADR.

### What `masterContext` Is Today

`DAGExecutor` maintains a `masterContext map[string]string` that accumulates all step results during an execution run. Every completed step merges its output into this map via `mergeStepResult`; successor steps receive a `cloneMap(masterContext)` snapshot at dispatch time. It has three structural problems:

1. **Unbounded growth.** The map grows monotonically throughout a plan run. Long plans produce a map whose serialized form exceeds LLM context windows. The Planner receives the full dump without any prioritisation.
2. **Flat, untyped structure.** Keys are step IDs; values are raw string blobs. A step output about a database connection is indistinguishable from one about a file path.
3. **No cross-session enrichment.** The `masterContext` is populated only by the current run. Facts established in prior sessions never reach the Planner or individual steps unless explicitly injected at startup.

### Scope Boundary

This ADR covers: the `WorkspaceStage` interface, both enrichment call sites (Planner + DAGExecutor), scene reconstruction, LLM-arbitrated conflict resolution in ConsolidatorAgent, and config fields. The Tier-1 pending channel from ADR-0015 is **not** the GWS — it is a write-staging buffer internal to `MemoryAgent`, not a workspace enrichment layer.

**Design Philosophy Note:** The biological mechanisms referenced in this ADR (Global Workspace broadcast, thalamic gating, hippocampal scene reconstruction) are architectural constraints that define the solution space, not decorative metaphors. The GWS models the thalamo-cortical broadcast described in Global Workspace Theory (Baars, 1988): bounded enrichment from LTM is broadcast to all downstream processes (Planner, DAGExecutor) without restructuring those processes. This ADR assumes the biological-grounding philosophy is accepted; its purpose is to specify the implementation and its empirical validation path (SPC metrics, truncation telemetry), not to re-justify the philosophy.

---

## Decisions

### 1. Enrichment Model — Not Full Replacement

The `masterContext map[string]string` is **retained** for intra-run step dependency flow. Step results written by `mergeStepResult` are never evicted mid-run — the current `step_i_result` / `step_i_{k}` key convention is preserved. The `DependsOn` contract in `TopologicalSort` relies on successors finding their predecessors' results in every snapshot; evicting step results mid-run would silently break this.

The GWS is an **additive bounded enrichment layer**: before each step's `cloneMap(masterContext)` snapshot is cut, cross-session LTM facts and scene-reconstructed context are merged into the map under `ltm_*` prefixed keys. The "bounded" constraint applies only to this LTM-enriched portion — not to step results.

This means DAGExecutor's blast radius from this ADR is minimal: one nullable field added, one call at the top of `Execute`. The core execution loop (`dispatch`, `mergeStepResult`, replan, checkpoint) is untouched.

### 2. `WorkspaceStage` Interface in `internal/domain/`

A new interface is added to `internal/domain/`:

```go
type WorkspaceStage interface {
    PrimeForPlanning(ctx context.Context, taskQuery string) (map[string]string, error)
    PrimeForExecution(ctx context.Context, plan *ExecutionPlan, initialContext map[string]string) (map[string]string, error)
}
```

This interface is shared between the Planner (awareness layer) and DAGExecutor (metabolism layer). Both already import `internal/domain/`. Defining it locally in each consuming package would produce two disconnected definitions of the same concept; a domain-level definition is the correct home for a shared capability boundary.

The implementation lives in `internal/memory/` and satisfies the interface.

### 3. Two Enrichment Touch Points

The GWS enriches at two distinct points in the execution lifecycle, not one:

**Touch Point 1 — Pre-Planner:**
`Planner.GetExecutionPlan` calls `WorkspaceStage.PrimeForPlanning(ctx, taskQuery)` before building the system prompt. The returned map is injected into the Planner's context window alongside the capability cluster block and hippocampus templates. The Planner is now globally workspace-aware: it receives cross-session facts about how similar tasks have resolved in past runs, producing higher-quality step decompositions.

**Touch Point 2 — Execution Start:**
At the top of `DAGExecutor.Execute` and `ExecuteFrom`, before `masterContext := cloneMap(initialContext)`, the executor calls `WorkspaceStage.PrimeForExecution(ctx, plan, initialContext)`. The returned enriched map replaces `initialContext` going into `cloneMap`. All steps in the run inherit the cross-session enrichment without per-step latency.

Both fields are nullable (same pattern as `EventWriter`, `CheckpointStore`, `ReplanHandler`). When nil, existing behaviour is preserved exactly — no breaking changes to callers or tests.

### 4. Scene Reconstruction in Both Enrichment Calls

Both `PrimeForPlanning` and `PrimeForExecution` run scene reconstruction before the LTM fact pull. The sequence inside each call:

1. **SCENE retrieval** — query pgvector for `DocTypeMnemonicScene` documents (ADR-0015) using the task query or plan description. This retrieves the environmental snapshot (directory state, active plan index, env vars) from the most relevant past execution.
2. **Embedding priming** — augment the query embedding with the SCENE context. This exploits encoding specificity: recreating the environmental context under which a memory was formed improves retrieval precision for the subsequent FACT pull.
3. **FACT pull** — query pgvector for `DocTypeMnemonicFact` documents using the primed embedding. Two-phase `cosine_similarity × activation_strength` re-ranking (ADR-0015) applies.

**Required implementation detail — parallel SCENE-fetch:** The SCENE query (step 1) MUST be launched concurrently with system-prompt assembly (BBolt cluster read + hippocampus template render). The SCENE query does not depend on the system-prompt content; running it sequentially adds an unnecessary pgvector round-trip to the critical path.

**Cold-start fallback:** If the SCENE retrieval returns zero results (new deployment, empty LTM, or cold session), skip the priming step and issue the FACT query with the raw query embedding (no priming). Log `workspace_cold_start=true, scene_hits=0, fact_hits=N, session_id=...` as structured fields. Never return an empty enrichment map — always return the best available FACT results. An empty map pays the full latency cost of two pgvector queries and delivers zero enrichment value; raw FACT results are the graceful degradation baseline (pre-ADR-0016 behaviour).

**Scene priming drift guard (off by default, schema-enabled):** SCENE documents retrieved from prior sessions may reflect a different but similar task, causing systematic priming drift — the embedding shifts toward a different task domain with each enrichment call. A drift guard is schema-enabled via `workspace_enable_drift_guard bool` (default `false`) in `ExecutionConfig`. When enabled: compute the cosine similarity between the primed embedding and the raw query embedding; if similarity falls below `workspace_drift_threshold` (default 0.7), discard SCENE priming and revert to raw query embedding. Log `scene_priming_session_overlap_rate` as a structured field per enrichment call.

Two pgvector queries per enrichment call (one SCENE, one FACT). SCENE documents never enter the returned map as evidence — they are retrieval-priming artefacts only, consistent with ADR-0015's confabulation guard.

### 5. Pre-planning Fact Contradiction Guard

Before `PrimeForPlanning` builds the enrichment map, it performs a pairwise cosine similarity check across the top-K FACT results:

1. Compute cosine similarity between all pairs in the FACT result set (vector operation — near-zero cost).
2. For any pair exceeding similarity threshold `0.85`, issue a single LLM AGREE/CONFLICT token call to classify the pair.
3. If `CONFLICT`: tag both documents with `[CONFLICT: doc_A vs doc_B]` in the enrichment map metadata and include both in the returned map. The Planner receives the conflict tag inline with the evidence.

Conflicts are **disclosed, not resolved**. The Planner prompt receives `[CONFLICT]` tags; arbitration is the LLM's responsibility. Plans are never blocked on unresolved conflicts. `SemanticCheckpoint` (ADR-0013) handles mid-execution incoherence independently — if contradictory context causes execution drift, the checkpoint catches it via cosine similarity thresholding on consecutive step outputs.

The AGREE/CONFLICT LLM call fires **only** when a near-duplicate pair (similarity > 0.85) is detected. In the absence of near-duplicate high-similarity pairs, zero LLM calls are added to the critical path.

### 6. LLM-Arbitrated Conflict Resolution in ConsolidatorAgent

Semantic conflicts between LTM-enriched facts and current-run step outputs are resolved at **session consolidation time** — not during DAG execution. During execution, LTM-enriched `ltm_*` keys and step-result `step_i_*` keys are namespaced and do not collide directly.

At consolidation time, `BuildConsolidationPrompt` (`internal/awareness/consolidator_agent.go`) is extended with a `BuildReconsolidationPrompt` function. When the ConsolidatorAgent detects a semantic contradiction — two facts about the same entity with contradicting content — it invokes the Generator with the reconsolidation prompt, providing:

- Both contradicting facts (text + key)
- Credibility signals for each: `activation_strength`, `access_count` (LTM side); agent TrustScore, `TaskEvent.Verified bool` (step side)

The LLM reasons about which fact is correct and produces a winner plus an explanation. The winner is written back to LTM; the loser is either dropped or downgraded in `activation_strength`. This runs at consolidation time — not on the hot DAG execution path.

### 7. Two Configurable Enrichment Slot Counts

Following ADR-0014's pattern (four new `ExecutionConfig` fields), two new fields are added:

```json
"workspace_planning_slots": 10,
"workspace_execution_slots": 5
```

`PrimeForPlanning` pulls top-10 FACT documents. `PrimeForExecution` pulls top-5. The Planner receives a richer cross-session context (plan quality benefits from breadth); execution enrichment is kept lean (every slot becomes a key in every step's snapshot via `cloneMap`).

**WIP limit derivation:** These slot counts are context-budget constraints, not throughput constraints. At ~200 tokens per FACT document, 10 planning slots consume ~2,000 tokens — a bounded, predictable enrichment overhead within typical LLM context budgets. 5 execution slots is the minimum set for tool-parameter grounding without overloading the step prompt. These are design hypotheses; the truncation telemetry field (below) is the production validation mechanism.

**Truncation telemetry (required):** `PrimeForPlanning` and `PrimeForExecution` MUST log `workspace_slots_truncated=true, available_docs=N, slots=M` whenever the relevance-ranked candidate list exceeds the configured slot limit. This is the SPC signal that the WIP limit is the active bottleneck. If `truncated=true` in >20% of calls over a 7-day window, the operator SHOULD increase the slot count and monitor cold-start latency.

### 8. Nullable Field Migration — No Breaking Changes

`WorkspaceStage` is added as a nullable field to both consuming structs:

```go
// In DAGExecutor:
WorkspaceStage domain.WorkspaceStage // may be nil; nil disables GWS enrichment

// In Planner (awareness layer):
WorkspaceStage domain.WorkspaceStage // may be nil; nil disables pre-planning enrichment
```

When nil, `Execute` and `GetExecutionPlan` behave exactly as today. Existing tests require no changes. Kernel wiring (`bootstrapKernel`) sets the field when the implementation is available — following the same pattern as `SweepTrigger` wiring in ADR-0014 slice 0014-07.

---

## Considered Options

**Full masterContext replacement with DAG-aware eviction.** The GWS would replace `masterContext` entirely and track plan topology to determine which step results are eligible for eviction (only once all declared successors have been dispatched). This would require the GWS to hold a reference to the plan and track `completed` state — significant structural coupling into the core execution loop. Rejected: the enrichment model achieves the cross-session awareness goal with zero changes to the execution loop.

**Per-step Spotlight trigger.** The Spotlight runs inside the `dispatch()` loop before each `cloneMap`, enriching every step's snapshot with LTM facts relevant to that specific step's query. Maximum precision; each step gets the most targeted cross-session context. Rejected: adds 50–200ms pgvector latency before every step dispatch, cumulative across the full plan. Once-at-execution-start enrichment provides the cross-session benefit without per-step latency.

**gRPC broadcast to all registered agents.** The GWS pushes state updates to all audience agents as the plan progresses. Rejected: complexity scales as O(N · K · M) — network degradation and CPU thrashing under high-concurrency plans.

**Two separate consumer-side interfaces (local per package).** Each consuming package defines its own minimal `WorkspaceStage` interface; Go structural typing satisfies both from one implementation. Rejected: `WorkspaceStage` is genuinely shared between two architectural layers — a domain-level definition is the correct home.

**Formula-based conflict resolution.** A deterministic score `step_wins = step.Verified && agentTrustScore × 1.5 > ltm.activation_strength`. Rejected: weights are arbitrary; getting them wrong means a hallucination silently overwrites validated knowledge. The Generator is already wired into the awareness layer and handles edge cases a formula cannot anticipate.

**Pre-Planner scene reconstruction only.** Scene reconstruction runs only before `GetExecutionPlan`. Rejected: the execution enrichment path (`PrimeForExecution`) would then operate without scene-primed embeddings, reducing retrieval precision during the execution-start LTM pull. Both touch points benefit from the same scene priming sequence.

**Plan blocking on unresolved contradiction.** If `PrimeForPlanning` detects a `[CONFLICT]` pair, block the plan until the contradiction is resolved. Rejected: contradiction resolution requires a Generator call that is not on the critical path; blocking execution on a consolidation-time operation introduces unplanned latency. Disclosure (`[CONFLICT]` tag inline) gives the Planner the information without blocking it. `SemanticCheckpoint` handles any downstream incoherence.

**Empty enrichment map as cold-start return.** Return an empty map when SCENE hits = 0. Rejected: an empty map pays the full latency cost of two pgvector queries and delivers zero enrichment; it is a hard failure mode masquerading as a fallback. Raw FACT results are the correct graceful degradation — identical to pre-ADR-0016 behaviour. Logging `workspace_cold_start=true` makes the unprimed state observable for SPC without penalising the caller.

---

## Consequences

- `internal/domain/` gains `WorkspaceStage` interface. No existing domain types are changed.
- `DAGExecutor` gains one nullable `WorkspaceStage` field and one call at the top of `Execute`/`ExecuteFrom`. The core execution loop — `dispatch`, `mergeStepResult`, replan, checkpoint, semantic gate — is untouched.
- `Planner.GetExecutionPlan` gains one nullable `WorkspaceStage` field and one call before the system prompt is built.
- `internal/memory/` gains a new `WorkspaceStageImpl` type implementing both interface methods (scene retrieval + FACT pull); SCENE query launches concurrently with system-prompt assembly (required implementation detail).
- `internal/awareness/consolidator_agent.go` gains `BuildReconsolidationPrompt` — a contradiction-handling extension alongside the existing `BuildConsolidationPrompt`.
- `ExecutionConfig` gains four new fields: `WorkspacePlanningSlots int` (default 10), `WorkspaceExecutionSlots int` (default 5), `WorkspaceEnableDriftGuard bool` (default false), `WorkspaceDriftThreshold float64` (default 0.7).
- Structured log fields emitted per enrichment call: `workspace_cold_start`, `scene_hits`, `fact_hits`, `workspace_slots_truncated`, `available_docs`, `slots`, `scene_priming_session_overlap_rate` (when drift guard enabled).
- No existing callers, tests, or interfaces break. The nullable field pattern guarantees behavioural equivalence when the field is unset.
- ADR-0015 Phase 1 (DB schema, `activation_strength` column, HNSW index rebuild, stored procedures, pg_cron schedule) must be live before `WorkspaceStageImpl` can be deployed — it depends on `DocTypeMnemonicScene`, `DocTypeMnemonicFact`, and the cosine retrieval stored procedure.

---

## Implementation Slices (Proposed)

| Slice | Description | Blocked by |
|-------|-------------|------------|
| 0016-01 | `WorkspaceStage` interface in `internal/domain/`; nullable fields on `DAGExecutor` and `Planner`; nil-path tests confirm no behaviour change | ADR-0015 Phase 1 |
| 0016-02 | `WorkspaceStageImpl` in `internal/memory/`: parallel SCENE retrieval + primed FACT pull; cold-start fallback; two config fields; unit tests with mock pgvector | 0016-01 |
| 0016-03 | Planner `PrimeForPlanning` wiring: inject enriched map into system prompt; contradiction guard (cosine similarity + AGREE/CONFLICT token call); test with mock WorkspaceStage | 0016-02 |
| 0016-04 | DAGExecutor `PrimeForExecution` wiring: call at top of Execute/ExecuteFrom; integration test confirms `ltm_*` keys appear in step snapshots | 0016-02 |
| 0016-05 | `BuildReconsolidationPrompt` in `consolidator_agent.go`: contradiction detection + LLM arbitration prompt; kernel wiring | 0016-03, 0016-04 |
| 0016-06 | Scene priming drift guard: `workspace_enable_drift_guard` config; cosine similarity check between primed and raw embeddings; `scene_priming_session_overlap_rate` telemetry | 0016-02 |
