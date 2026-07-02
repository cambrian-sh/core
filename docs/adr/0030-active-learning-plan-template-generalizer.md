# ADR-0030: Active Learning — Plan Template Generalizer

## Status

Implemented

## Context

The `Hippocampus` stores successful `ExecutionPlan` records as `DocTypeProceduralTemplate` documents in pgvector (ADR-0027). It retrieves them by cosine similarity at planning time. However, it never:

- Promotes repeatedly-successful plans into reusable generalised templates
- Suppresses plans that consistently fail
- Distinguishes a one-off execution from a validated, repeatable pattern

The result is a flat store of raw executions with no signal about which plans are reliable versus which are reliably broken. `REQ-BRAIN-3` specifies a `PlanTemplateGeneralizer` to address this.

**Deviations from REQ-BRAIN-3 (grilling session, 2026-06-01):**

| Spec assumption | Decision |
|---|---|
| Triggered by nightly `CircadianRhythm` job | Triggered by `MemoryPressureEvent` (REQ-CIRCADIAN-001) — no new ticker |
| "Groups by `plan_hash`" — field not defined | `plan_hash = SHA-256(normalise(canonicalKey))` with cosine fallback for LLM text drift |
| NegativeEdge in `document_edges` to suppress bad plans | `blacklisted: true` metadata flag + filter in `RetrieveWithPolicy` — graph edge gives no retrieval suppression |
| "Writes to config" for threshold adjustment (behavior 5) | **Dropped** — emits `slog.Info` calibration hints only; `HippocampusPolicy` is operator-controlled |
| Raw plans pruned when template promoted | Not pruned — generalizer guards on `is_template: true`, raw plans become inert behind the template |

---

## Decision

### 1. Plan Identity: Two-Level Grouping

Plan identity is computed in two ordered passes:

**Pass 1 — Exact hash:**
```
plan_hash = SHA-256(normalise(canonicalKey))
canonicalKey = Hippocampus.canonicalKey(plan)  // "intent: {subject} | step: ..."
normalise() = existing Hippocampus normalise() function (lowercase + punctuation strip)
```

Plans sharing the same `plan_hash` are structurally identical. This is the fast path — no embedding needed.

**Pass 2 — Cosine fallback (threshold ≥ 0.93):**
For plans that don't accumulate enough count by exact hash, embed the canonical key and group with any existing `DocTypeProceduralTemplate` whose cosine similarity ≥ 0.93. This handles minor LLM text drift on structurally equivalent plans (e.g., "sort the array" vs "sort array in ascending order").

The 0.93 threshold is tighter than any existing `HippocampusPolicy` threshold to prevent false grouping of semantically similar but structurally different plans. It is configurable via a new `generalization_cosine_threshold` field in `ExecutionConfig` (default 0.93).

### 2. Trigger: MemoryPressureEvent

The `PlanTemplateGeneralizer` subscribes to `MemoryPressureEvent` (REQ-CIRCADIAN-001). When the pgvector index exceeds `ConsolidationThresholdDocCount`, the event fires and the generalizer runs.

**No new ticker is introduced.** This is consistent with REQ-CIRCADIAN-001's event-driven model. The generalizer is registered alongside the `LazyConsolidator` as a `MemoryPressureEvent` handler.

### 3. Scan Scope: Watermark in BBolt

Each `PlanTemplateGeneralizer` run processes only PlanEvents newer than its last watermark:

```go
// New BBolt key in a dedicated "generalizer_state" bucket:
key: "plan_generalizer_watermark"
value: time.Time (RFC3339, last successfully processed timestamp)
```

**New BBolt read method required:**
```go
func (b *BBoltAdapter) ReadPlanEventRecordsSince(since time.Time) ([]PlanEventRecord, error)
```
Implemented as a `ForEach` cursor scan over the `plan_events` bucket, filtering by `StartTime >= since`. After each successful generalizer run, the watermark is advanced to `now`.

On first run (no watermark): processes all PlanEvents. Safe — the generalizer is idempotent when `is_template: true` guard is in place.

### 4. Generalization: Direct Promotion vs. LLM Abstraction

**Exact-hash groups** (identical canonical keys): Promote the plan directly. No LLM call. Set `is_template: true` in `Document.Metadata`.

**Cosine-similarity groups** (structurally similar, textually different): Send the N representative plans (up to 5) to the LLM with a registered abstraction prompt. The LLM replaces differing parameter values with `{variable_name}` placeholders. The abstracted plan is stored as a new `DocTypeProceduralTemplate` with `is_template: true`.

The abstraction prompt is registered in `domain.PromptRegistry` per PROMPTREQ:
```go
domain.PromptRegistry[abstractionPromptHash] = domain.PromptEntry{
    ID:      "planner.plan_abstraction",
    Version: "1.0.0",
    Hash:    abstractionPromptHash,
    Schema:  abstractionSchema,
}
```

### 5. Generalizer Guard: `is_template` Flag

Before processing any group, the generalizer checks whether a `DocTypeProceduralTemplate` with `is_template: true` already covers that hash/cosine group. If yes — skip.

This means:
1. Raw plans behind a promoted template are never re-processed
2. Raw plans are never deleted — they become inert, naturally outscored by the template at retrieval time
3. The generalizer is safe to run repeatedly without double-promoting

### 6. Thresholds for Promotion and Blacklisting

| Signal | Threshold | Configurable via |
|---|---|---|
| Promote to template | ≥3 executions AND ≥90% `PlanOutcome = success` | `generalization_min_executions`, `generalization_success_rate` in `ExecutionConfig` |
| Blacklist | ≥3 executions AND ≤30% `PlanOutcome = success` | `generalization_blacklist_rate` in `ExecutionConfig` |
| Cosine fallback grouping | ≥0.93 cosine similarity | `generalization_cosine_threshold` in `ExecutionConfig` |

### 7. Blacklisting: Metadata Flag in Hippocampus

When a plan group meets the blacklist threshold (≥3 executions, ≤30% success):

1. Set `blacklisted: true` in the `DocTypeProceduralTemplate` document's `Metadata`
2. Emit `slog.Warn("PLAN_TEMPLATE_BLACKLISTED", "plan_hash", hash, "failure_rate", rate)`
3. `Hippocampus.RetrieveWithPolicy` filters blacklisted documents:

```go
if blacklisted, _ := results[0].Document.Metadata["blacklisted"].(bool); blacklisted {
    return nil, 0, 0, nil  // treat as miss
}
```

**Why not `DocTypeNegativeEdge` in `document_edges`?** The `Hippocampus.RetrieveWithPolicy` path never consults `document_edges`. More critically, the REQ-CACHE-1 fast-path in `GetExecutionPlan` returns a matching plan *before* any Planner prompt is built — `<NegativeLTM>` injection is never reached. The metadata flag is the only suppression mechanism that fires at the right level.

A blacklisted template is **recoverable**: an operator can clear the `blacklisted` flag manually. It is not deleted.

### 8. Behavior 5 Dropped: No Config Mutation

REQ-BRAIN-3 behavior 5 ("computes empirical `CachePolicy` thresholds and writes to config") is **dropped**.

Runtime mutation of `config.json` creates unversioned config drift invisible to operators. The `HippocampusPolicy` system (ADR-0027) is the correct operator-facing mechanism for threshold tuning.

Instead, the generalizer emits per-policy calibration hints after each run:
```go
slog.Info("plan_generalizer_policy_calibration_hint",
    "policy", policyName,
    "cache_hit_rate", hitRate,
    "cache_miss_rate", missRate,
    "suggestion", "consider lowering SimilarityThreshold if miss rate > 0.3")
```

These hints are observable in logs and can inform manual `config.json` updates but never trigger them automatically.

---

## Consequences

### What Becomes Possible

- Repeated successful plans are automatically elevated to reusable procedural templates, reducing LLM calls over time (feeds REQ-CACHE-1 fast-path).
- Consistently failing plans are suppressed at the Hippocampus level before they waste planning tokens.
- The `PlanTemplateGeneralizer` runs only when there is measurable memory pressure — zero CPU cost during idle periods.
- Operators receive log-level calibration hints about policy effectiveness without automatic config mutation.

### What Becomes Harder

1. **New BBolt read method**: `ReadPlanEventRecordsSince(since time.Time)` requires a ForEach cursor scan — more expensive than the existing single-record lookup. For large plan corpora, this scan may be slow. Mitigation: the watermark bounds the scan to only new records on subsequent runs.
2. **Watermark state**: If the generalizer fails mid-run, the watermark is not advanced and the same events are re-processed on the next run. This is safe (idempotent) but means some events are evaluated twice. Mitigation: only advance the watermark after a fully successful run.
3. **LLM cost for cosine groups**: One LLM abstraction call per cosine-similarity group found. This is bounded (only fires for structurally similar but textually different plans) and cheap relative to planning tokens saved.

### Known Gaps (Deferred)

- `PlanEvent` does not currently carry `plan_hash`. It must be computed at generalizer scan time from `PlanEvent.Subject + PlanEvent.Steps` — but `PlanEventRecord` only stores the serialised plan subject and outcomes, not the full step list. A `plan_hash` field should be added to `PlanEvent` at write time (DAGExecutor) and backfilled in the existing BBolt records. This is a prerequisite for efficient grouping.
- Fuzzy semantic grouping of plans with different structure (Scenario C from grilling) is explicitly deferred.

### Rejected Alternatives

| Alternative | Why Rejected |
|---|---|
| Nightly `CircadianRhythm` ticker | Violates REQ-CIRCADIAN-001's event-driven model; misses backlog on restarts |
| Configurable lookback window (rolling 7 days) | Simpler but processes duplicate events on restarts; watermark is strictly better |
| NegativeEdge in `document_edges` for blacklisting | `RetrieveWithPolicy` never reads `document_edges`; REQ-CACHE-1 fast-path bypasses the Planner entirely |
| Regex-based plan abstraction | Misses semantic variable names; LLM produces higher-quality templates |
| Threshold auto-adjustment (behavior 5) | Unversioned config drift; `HippocampusPolicy` already provides the correct operator-facing tuning mechanism |
| Prune raw plans behind promoted template | Unnecessary — `is_template` guard makes raw plans inert; deletion adds failure-mode complexity |

---

## Implementation Notes

### New Domain Types

- `domain.PlanTemplateGeneralizer` interface — `Run(ctx context.Context) error`
- `PlanEvent.PlanHash string` field (SHA-256 of normalised canonical key, populated at DAGExecutor write time)

### New Config Fields (`ExecutionConfig`)

| Field | Default | Purpose |
|---|---|---|
| `GeneralizationMinExecutions` | `3` | Minimum executions before promotion/blacklist decision |
| `GeneralizationSuccessRate` | `0.90` | Success rate floor for template promotion |
| `GeneralizationBlacklistRate` | `0.30` | Success rate ceiling for blacklisting |
| `GeneralizationCosineThreshold` | `0.93` | Cosine similarity floor for fallback grouping |

### New BBolt

- Bucket `"generalizer_state"` — key `"plan_generalizer_watermark"` stores last-processed `time.Time`
- `BBoltAdapter.ReadPlanEventRecordsSince(since time.Time) ([]PlanEventRecord, error)`
- `BBoltAdapter.WriteGeneralizerWatermark(t time.Time) error`
- `BBoltAdapter.ReadGeneralizerWatermark() (time.Time, error)`

### Hippocampus Change

Single new condition in `RetrieveWithPolicy` after cosine/confidence/age checks:
```go
if blacklisted, _ := results[0].Document.Metadata["blacklisted"].(bool); blacklisted {
    return nil, 0, 0, nil
}
```

### Activation Path

```
pgvector index exceeds ConsolidationThresholdDocCount
    → MemoryPressureEvent published
    → PlanTemplateGeneralizer.Run(ctx) invoked
    → Read watermark from BBolt
    → ReadPlanEventRecordsSince(watermark)
    → Group by plan_hash (exact) then cosine (fallback)
    → For groups meeting thresholds: promote or blacklist
    → Advance watermark
    → Emit calibration hints via slog.Info
```

---

## Related

- ADR-0012 (Session lifecycle)
- ADR-0015 (Hippocampus, DocTypeProceduralTemplate, activation_strength)
- ADR-0027 (HippocampusPolicy — named threshold configuration)
- ADR-0029 (EpisodicMemory — same MemoryPressureEvent subscription model)
- CAMBRIAN-OS.md §4.3 (REQ-BRAIN-3)
- REQ-CIRCADIAN-001 (MemoryPressureEvent, event-driven lifecycle)
