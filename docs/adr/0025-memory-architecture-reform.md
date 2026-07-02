# ADR-0025: Memory Architecture Reform — LTM Quality, Format Standard, and Live Graph

The LTM retrieval pipeline was returning irrelevant memories because untyped `DocTypeMemory` documents (raw step outputs, error dumps) were accumulating in pgvector and dominating the floor-multiplier re-ranker. This ADR formalises seven interconnected decisions that fix the data pipeline, standardise the Planner injection format, and make the graph live during execution.

## Decisions

### 1. Retire `DocTypeMemory`

`DocTypeMemory = "memory"` is retired as a write target. No new documents may be stored with this type. Existing `'memory'` rows in pgvector are left to decay and GC via the Ebbinghaus stored procedure (`apply_ebbinghaus_decay`). The three callers that queried `DocTypeMemory` (`MemoryManager.Query`, `MemoryAgent.Query` Pass 2, `QueryService.Search`) are updated or removed as part of the `FetchContext` retirement below.

### 2. Retire `FetchContext` as a Planning Path

`MemoryAgent.FetchContext` is removed from `Server.Execute`'s pre-planning step. `WorkspaceStageImpl` becomes the **single LTM-to-Planner gate**. The poisoned memory warning (formerly `CRITICAL FAILURES TO AVOID`) is migrated to the `<NegativeLTM>` section of the `WorkspaceStage` enrichment — `WorkspaceStage.enrich` now also queries `DocTypeNegativeEdge` and includes matching documents in the enrichment map.

We considered keeping both paths with `FetchContext` redirected to query `DocTypeMnemonicFact`, but that produces duplicate retrieval on every plan request for no additional signal.

### 3. Heuristic Error Pre-filter Before Tier-2

Before the Tier-2 LLM-as-Judge batch runs, each pending item is checked against deterministic error signals:
- Prefix patterns: `BLOCKED:`, `FAILURE:`, `ERROR:`
- JSON fields: `"exit_code"` with a non-zero value, `"stderr"` non-empty
- Exception names: `SyntaxError`, `ModuleNotFoundError`, `Traceback`

Items matching any signal are routed immediately to `IngestNegativeEdge` with a standardised error document schema (`error_type`, `raw_output`, `agent_id`, `step_index`, `timestamp`) and excluded from the LLM batch. This is consistent with how `PauseController.NeedsIntervention` handles safety-critical patterns — deterministic signals never need LLM reasoning.

We rejected adding `NEGATIVE_EDGE` as a 5th LLM-scored tier because error patterns are syntactically deterministic and burning an LLM call to classify a syntax error is wasteful.

### 4. `DocTypeMnemonicScene` Written After Every Successful Step

A shallow `DocTypeMnemonicScene` document is written by `DAGExecutor` after every step that completes without an error signal (i.e., not routed to `IngestNegativeEdge`). Content: `step_query`, `agent_id`, `result_summary` (first 200 chars of output), `step_index`, `session_id`. This decouples scene creation from the Tier-2 scorer's FULL threshold (≥7), which in practice was never reached, leaving the scene corpus permanently empty and the WorkspaceStage SCENE query always returning cold.

### 5. Live Graph Edges During Execution

Two `document_edges` edge types are now written during live execution (not only at sleep consolidation):

| Edge | Source | Target | Written by |
|---|---|---|---|
| `specifies` | `scene_N` | `scene_{N-1}` | `DAGExecutor` at scene write time |
| `discussed_in` | `fact` (Tier-2 committed) | `scene` that produced the step | Tier-2 drain (scene ID carried in `pendingItem` metadata) |

`ConsolidatorAgent` retains sole authority over `contradicts` and `closes` edges, which require LLM semantic reasoning across sessions. The `specifies` and `discussed_in` edges are deterministic and require no LLM call.

### 6. Planner LTM Format Standard — XML Tags

The flat `ltm_fact_N: ...` key-value dump and the untagged `PRIOR SUCCESSFUL PLAN` block are replaced by a typed XML-tag standard:

```xml
<PlanLTM similarity="0.87" confidence="0.85" outcome="success" replan_count="0">
  {"subject": "...", "steps": [...]}
</PlanLTM>

<FactLTM>
  <fact id="0" activation="0.72">auth uses JWT for session tokens</fact>
  <fact id="1" activation="0.61">deployment target is k8s on eu-west-1</fact>
</FactLTM>

<NegativeLTM>
  <failure agent="terminal_agent">BLOCKED: 'write' is not in ALLOWED_COMMANDS</failure>
</NegativeLTM>
```

Each section is omitted entirely when empty — no cold-start noise in the prompt. `Hippocampus.Store` is updated to accept `plan_outcome` and `replan_count` from `PlanEvent` so the `<PlanLTM>` attributes carry review data, not just confidence.

We considered markdown headers and a single JSON envelope. XML-style tags were chosen because the LLM parses them as semantically distinct blocks (consistent with existing `<thought>` block parsing in the Planner), and attributes allow metadata without nesting.

### 7. REQ1 Integration Test — Three-Layer Relevance Gate

A new test `internal/memory/ltm_integration_test.go` (`//go:build e2e`) seeds pgvector with known-relevant and known-irrelevant documents (mirroring the exact failure examples in MEMORYREQ.md). Three assertions run against every query:

1. **Poison exclusion**: the known-irrelevant document (event sourcing/CRUD content) is not present in the returned result set for an unrelated query (prime numbers, atomic file writes).
2. **Cosine threshold**: every returned document scores ≥ 0.35 cosine similarity against the query embedding.
3. **LLM-as-Judge**: the LLM is asked `"Is this fact relevant to the query? YES or NO"` for each returned document. All must return YES.

Requires a live pgvector instance and embedder (same as existing `//go:build e2e` tests).
