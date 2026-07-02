# ADR-0021: System Quality Measurement & Deferred Data Pipeline

**Status:** Implemented  
**Date:** 2026-05-29  
**Depends on:** ADR-0018 (TokenUsage in TaskEvent), ADR-0019 (TelemetryObserver), ADR-0020 (Benchmarks + Chaos)  
**Scope:** Close the gap between "does it work" and "is it good". Measure token efficiency, end-to-end latency, and plan quality. Build data pipelines that feed all 7 deferred work items in `FUTURE_WORK_SUMMARY.md`.

---

## 1. Problem Statement

ADR-0020 answered: "Does the kernel crash, leak goroutines, or regress algorithmic throughput under load?"

ADR-0021 answers: "How good are the plans? How expensive are they? How fast do they finish with a real LLM? And are we collecting the data we will need to train the 7 deferred models?"

### 1.1 Gaps Discovered During Investigation

**Data Loss Bug (P0):** `domain.TaskEvent` carries `PromptTokens`, `CompletionTokens`, `TotalTokens`, `EstimatedCost`, `BudgetOverrun`, `FallbackModelUsed`, and `ActualModelID` (ADR-0018 / ADR-0019). `storage.TaskEventRecord` and `mapper.TaskEventToRecord` silently drop all of these fields. The bbolt store has never held a single token count. Every `TaskEvent` written to disk is truncated. This corrupts the `ProfileAggregator` token histogram, invalidates the `tools/export-events` corpus, and makes budget overrun SPC impossible.

**No Plan-Level Telemetry:** DAGExecutor writes per-step `TaskEvent`s but never aggregates them. There is no record of:
- Total tokens consumed by a plan
- Total latency from `Execute` to final handoff
- Number of replans, failed steps, or fallback model switches per plan
- Plan outcome (success / partial / replan-exhausted)

**No Retrieval Quality Data:** `WorkspaceStage` performs retrievals but logs only SPC metrics (`workspace_slots_truncated`, `scene_hits`, `fact_hits`). It does not record:
- Query embedding
- Retrieved document IDs with scores
- Which documents were consumed by which plan step
- Whether the plan succeeded after using those retrievals

**No Graph Trajectory Data:** `SpreadingEngine` performs BFS but logs only aggregate coverage (`bfs_graph_miss`, `edge_coverage`). It does not record individual edge traversals with plan outcomes — the prerequisite for Thompson Sampling.

**No Contradiction Resolution Data:** `ResolveContradiction` calls the LLM judge but logs only the winner ID. It does not record the feature vector `{ltm_as_delta, ltm_access_delta, agent_trust_delta, verified_flag, doc_age_delta, semantic_similarity}` that a future model would need.

**No End-to-End Quality Benchmark:** All existing benchmarks use fake LLMs (microsecond responses). No benchmark measures actual plan quality with `qwen3:8b`, actual token consumption, or actual wall-clock latency.

---

## 2. Goals & Non-Goals

### Goals
1. **Fix the TaskEventRecord data loss bug** — restore all ADR-0018/0019 fields to bbolt persistence.
2. **Plan-level telemetry** — `PlanEvent` struct aggregating step-level data; plan latency, total tokens, outcome, replan count.
3. **Retrieval session logging** — `RetrievalSession` capturing query embedding, retrieved docs, plan linkage; feeds LTR + nDCG@K deferred work.
4. **Graph traversal logging** — `TraversalLogEntry` per BFS edge traversal with retroactive outcome update; feeds Thompson Sampling.
5. **Contradiction logging** — `ContradictionResolution` with full feature vector; feeds conflict resolution model.
6. **End-to-end quality benchmark** — `internal/benchmarks/e2e_quality_test.go` using real `qwen3:8b` over Ollama; measures plan validity, token efficiency, wall-clock latency.
7. **Export + analysis pipeline** — extend `tools/export-events` for new data types; extend notebook for plan quality metrics.

### Non-Goals
- Implementing the 7 deferred models themselves (LTR, nDCG@K benchmark, Thompson Sampling, etc.). This ADR only builds the **data pipelines and prerequisites**.
- Real-time online learning. All deferred data is logged and exported offline.
- Changing plan generation logic. This ADR is purely observational.

---

## 3. Design

### 3.1 TaskEventRecord Fix (Issue 0021-01)

Add missing fields to `storage.TaskEventRecord`:
```go
PromptTokens      int     `json:"prompt_tokens,omitempty"`
CompletionTokens  int     `json:"completion_tokens,omitempty"`
TotalTokens       int     `json:"total_tokens,omitempty"`
EstimatedCost     float64 `json:"estimated_cost,omitempty"`
BudgetOverrun     bool    `json:"budget_overrun,omitempty"`
FallbackModelUsed bool    `json:"fallback_model_used,omitempty"`
ActualModelID     string  `json:"actual_model_id,omitempty"`
```

Update `mapper.TaskEventToRecord` and `mapper.TaskEventToDomain` to map these fields bidirectionally.

Update `tools/export-events` schema to `schema_version: 2` (additive fields, backward-compatible).

### 3.2 PlanEvent (Issue 0021-02)

**Domain type:**
```go
type PlanEvent struct {
    PlanID            string    `json:"plan_id"`
    Subject           string    `json:"subject,omitempty"`
    StepCount         int       `json:"step_count"`
    Outcome           PlanOutcome `json:"outcome"` // success, partial, replan_exhausted, budget_exceeded
    TotalPromptTokens int       `json:"total_prompt_tokens"`
    TotalCompletionTokens int   `json:"total_completion_tokens"`
    TotalTokens       int       `json:"total_tokens"`
    TotalEstimatedCost float64 `json:"total_estimated_cost"`
    ReplanCount       int       `json:"replan_count"`
    FailedStepIndex   int       `json:"failed_step_index,omitempty"`
    FallbackCount     int       `json:"fallback_count"` // steps that used fallback model
    BudgetOverrunCount int      `json:"budget_overrun_count"`
    StartTime         time.Time `json:"start_time"`
    EndTime           time.Time `json:"end_time"`
    DurationMs        int64     `json:"duration_ms"`
    // LTM enrichment metadata
    RetrievalSessionID string   `json:"retrieval_session_id,omitempty"`
}

type PlanOutcome string
const (
    PlanOutcomeSuccess          PlanOutcome = "success"
    PlanOutcomePartial          PlanOutcome = "partial"          // completed with errors, no replan possible
    PlanOutcomeReplanExhausted  PlanOutcome = "replan_exhausted" // replanned but still failed
    PlanOutcomeBudgetExceeded   PlanOutcome = "budget_exceeded"
)
```

**Writer interface:**
```go
type PlanEventWriter interface {
    WritePlanEvent(event PlanEvent) error
}
```

**Storage:** New bbolt bucket `plan_events`. Key = `PlanID`. JSON value = `PlanEventRecord` DTO.

**Hook point:** `DAGExecutor.ExecuteFrom` writes `PlanEvent` on return, aggregating all step results accumulated during execution. The aggregation state is maintained in a lightweight `planAccumulator` struct local to `ExecuteFrom`.

### 3.3 TelemetryObserver Extension

Add three new methods to `domain.TelemetryObserver`:
```go
OnPlanCompleted(event PlanEvent)
OnRetrievalCompleted(session RetrievalSession)
OnContradictionResolved(resolution ContradictionResolution)
```

Noop implementations are empty. This follows the ADR-0019 coarse-grained pattern.

### 3.4 RetrievalSession (Issue 0021-03)

**Domain type:**
```go
type RetrievalSession struct {
    SessionID       string            `json:"session_id"`
    Query           string            `json:"query"`
    QueryEmbedding  []float32         `json:"query_embedding,omitempty"`
    Caller          string            `json:"caller"` // "planning" or "execution"
    SceneHits       int               `json:"scene_hits"`
    FactHits        int               `json:"fact_hits"`
    RetrievedDocs   []RetrievedDoc    `json:"retrieved_docs"`
    Truncated       bool              `json:"truncated"`
    PlanID          string            `json:"plan_id,omitempty"`     // linked post-execution
    PlanOutcome     PlanOutcome       `json:"plan_outcome,omitempty"` // retroactively updated
    ExplorationSlot bool              `json:"exploration_slot"`        // 5% uniform sample from top-3K
    Timestamp       time.Time         `json:"timestamp"`
}

type RetrievedDoc struct {
    DocID            string  `json:"doc_id"`
    Score            float64 `json:"score"`
    ActivationStrength float64 `json:"activation_strength"`
    DocType          string  `json:"doc_type"`
    Rank             int     `json:"rank"`
}
```

**Logger interface:**
```go
type RetrievalSessionLogger interface {
    LogRetrieval(session RetrievalSession) error
    LinkToPlanOutcome(sessionID string, planID string, outcome PlanOutcome) error
}
```

**Hook point:** `WorkspaceStageImpl.enrich` constructs and logs a `RetrievalSession` after the FACT query completes. `PlanID` and `PlanOutcome` are filled retroactively by the DAGExecutor calling `LinkToPlanOutcome` after plan completion.

**Storage:** New bbolt bucket `retrieval_sessions`. Key = `SessionID`. Value = JSON.

**Exploration slot:** 5% of retrievals are flagged `exploration_slot=true`. These are a uniform random sample from the top-3K cosine hits (not just the top-N slots), ensuring diversity for future LTR training.

### 3.5 Graph Traversal Log (Issue 0021-04)

**Domain type:**
```go
type TraversalLogEntry struct {
    EntryID       string    `json:"entry_id"`
    SourceID      string    `json:"source_id"`
    TargetID      string    `json:"target_id"`
    EdgeType      string    `json:"edge_type"`
    EdgeWeight    float64   `json:"edge_weight"`
    TransferredEnergy float64 `json:"transferred_energy"`
    Depth         int       `json:"depth"`
    PlanID        string    `json:"plan_id,omitempty"`     // linked retroactively
    PlanOutcome   PlanOutcome `json:"plan_outcome,omitempty"` // retroactively updated
    Timestamp     time.Time `json:"timestamp"`
}
```

**Logger interface:**
```go
type TraversalLogger interface {
    LogTraversal(entry TraversalLogEntry) error
    UpdatePlanOutcome(entryID string, planID string, outcome PlanOutcome) error
}
```

**Hook point:** `SpreadingEngine.Spread` logs one entry per traversed edge inside the BFS loop. `PlanID` and `PlanOutcome` are updated retroactively by DAGExecutor.

**Storage:** New bbolt bucket `traversal_log`. Key = `EntryID` (UUID). Value = JSON.

### 3.6 Contradiction Resolution Log (Issue 0021-05)

**Domain type:**
```go
type ContradictionResolution struct {
    ResolutionID     string    `json:"resolution_id"`
    DocAID           string    `json:"doc_a_id"`
    DocBID           string    `json:"doc_b_id"`
    WinnerID         string    `json:"winner_id"`
    // Feature vector for future model training
    DocAAS           float64   `json:"doc_a_as"`
    DocBAS           float64   `json:"doc_b_as"`
    DocAAccessCount  int       `json:"doc_a_access_count"`
    DocBAccessCount  int       `json:"doc_b_access_count"`
    DocAAgeDays      int       `json:"doc_a_age_days"`
    DocBAgeDays      int       `json:"doc_b_age_days"`
    SemanticSimilarity float64 `json:"semantic_similarity"`
    // Contextual signals
    ConsolidatorAgentTrust float64 `json:"consolidator_agent_trust,omitempty"`
    VerifiedA          bool      `json:"verified_a,omitempty"`
    VerifiedB          bool      `json:"verified_b,omitempty"`
    Timestamp          time.Time `json:"timestamp"`
}
```

**Hook point:** `ResolveContradiction` constructs and logs a `ContradictionResolution` after parsing the winner. The feature vector is computed from the two `domain.Document` inputs before the LLM call, so the log is independent of the LLM's reasoning.

**Storage:** New bbolt bucket `contradiction_resolutions`. Key = `ResolutionID`. Value = JSON.

### 3.7 End-to-End Quality Benchmark (Issue 0021-06)

**File:** `internal/benchmarks/e2e_quality_test.go`

**Requirements:**
- Uses **real** `qwen3:8b` via Ollama on host (not fake LLM).
- Uses real PostgreSQL + pgvector (Docker Compose or existing instance).
- Skips gracefully if Ollama is unreachable (`t.Skip`).
- Tagged `//go:build e2e` (not run in standard CI; run manually or in nightly).

**Benchmarks:**
| Name | Task | Metrics |
|------|------|---------|
| `BenchmarkE2E_SimplePlan_3step` | "Sort a list of integers" | wall_ms, prompt_tokens, completion_tokens, plan_validity |
| `BenchmarkE2E_MultiAgentPlan_5step` | "Design a REST API for a todo app" | wall_ms, total_tokens, step_count_efficiency, replan_count |
| `BenchmarkE2E_LTMEnrichedPlan` | Task that benefits from LTM priming | wall_ms, retrieval_quality_correlation |

**Plan Quality Metrics:**
- `plan_validity`: did the plan execute without replan-exhaustion?
- `token_efficiency`: `total_completion_tokens / step_count` (lower is better)
- `step_count_efficiency`: actual steps vs minimal steps needed (LLM-as-judge)
- `retrieval_utility`: did the retrieved LTM docs actually help? (measured by comparing plan quality with/without WorkspaceStage)

**LLM-as-Judge for Plan Quality:**
A secondary `qwen3:8b` call evaluates the plan output against the original task description on a 1-5 rubric:
- 5 = complete, correct, concise
- 4 = complete with minor issues
- 3 = partially complete
- 2 = largely incorrect
- 1 = completely wrong or refused

The judge prompt is deterministic (temperature=0) and structured JSON output.

### 3.8 Export & Analysis Pipeline (Issue 0021-07)

**Schema Version Bump:** `tools/export-events` outputs `schema_version: 2` for `TaskEventRecord` (additive fields). New export commands:
- `--type plan` → exports `plan_events` bucket
- `--type retrieval` → exports `retrieval_sessions` bucket
- `--type traversal` → exports `traversal_log` bucket
- `--type contradiction` → exports `contradiction_resolutions` bucket

**Notebook extension:** `tools/telemetry-analysis/analysis.ipynb` gains cells for:
- Plan outcome distribution (success / partial / replan_exhausted / budget_exceeded)
- Token efficiency histogram
- Retrieval session → plan outcome correlation
- Exploration slot coverage check (verify 5% rate)

---

## 4. Implementation Order

1. **0021-01:** Fix TaskEventRecord (unblocks all downstream data)
2. **0021-02:** PlanEvent domain + storage + DAGExecutor hook
3. **0021-03:** RetrievalSessionLogger + WorkspaceStage hook
4. **0021-04:** TraversalLogger + SpreadingEngine hook
5. **0021-05:** ContradictionResolution + ConsolidatorAgent hook
6. **0021-06:** E2E quality benchmark
7. **0021-07:** Export CLI + notebook extension

---

## 5. Risks & Mitigations

| Risk | Mitigation |
|------|------------|
| Bbolt bucket proliferation (4 new buckets) | Each bucket is small JSON values; total size bounded by session volume. Archive old buckets via `tools/export-events` + delete. |
| E2E benchmark flakiness with real LLM | Skip on unreachable Ollama; run manually, not per-PR; judge temperature=0 for determinism. |
| Retroactive outcome linking complexity | Store `PlanID` in step's `TaskEvent` (already derived as `step-{index}-{planID}`); retrieval and traversal entries hold `PlanID` string field updated in a second bbolt write after plan completion. |
| Performance impact of logging | All log writes are best-effort (same pattern as `EventWriter.WriteTaskEvent`). Errors are slog.Warn, never blocking. |
| Exploration slot bias | Uniform random sample from top-3K, not top-N. Random seed fixed per-deployment for reproducibility. |

---

## 6. Acceptance Criteria

- [ ] `TaskEventRecord` roundtrip test: all fields survive bbolt write → read → domain conversion.
- [ ] `PlanEvent` is written after every `DAGExecutor.Execute` call; `DurationMs` matches wall clock.
- [ ] `RetrievalSession` logs contain `QueryEmbedding`, `RetrievedDocs`, and retroactive `PlanOutcome`.
- [ ] `TraversalLogEntry` count correlates with `SpreadingEngine.Spread` BFS depth.
- [ ] `ContradictionResolution` contains all 8 feature fields.
- [ ] E2E benchmark produces `wall_ms`, `total_tokens`, and `plan_validity` for at least 3 tasks with real `qwen3:8b`.
- [ ] `tools/export-events --type plan` produces valid JSONL with `schema_version: 2`.
- [ ] Notebook loads all 4 new data types without errors.
- [ ] `go build ./...` clean; `go test ./...` passes (excluding `//go:build e2e`); `go test -bench=. ./internal/benchmarks/...` passes.
- [ ] Separability gate passes: no `go.opentelemetry.io` imports in core packages.

---

## 7. Deferred Work Trigger Updates

| Deferred Item | New Trigger | Data Source |
|---------------|-------------|-------------|
| LTR Model | ≥ 100 retrieval sessions with linked plan outcomes | `retrieval_sessions` bucket |
| nDCG@K Benchmark | ≥ 100 retrieval sessions + labels | `retrieval_sessions` bucket + judge annotations |
| Thompson Sampling | ≥ 1,000 traversal log entries with outcomes | `traversal_log` bucket |
| Prompt Recalibration | `scoring_prompt_version` drift > 30% | existing `documents` table |
| Decay Sensitivity | 90 days production telemetry | `PlanEvent` + `TaskEvent` |
| Conflict Resolution Model | ≥ 50 contradiction resolutions | `contradiction_resolutions` bucket |
| Arize Phoenix | ≥ 1K real TaskEvents + baseline notebook | `tools/export-events` + `tools/telemetry-analysis/` |
