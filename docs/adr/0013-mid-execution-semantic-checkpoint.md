# ADR-0013: Mid-execution Semantic Checkpoint (Re-entry Gate)

**Status:** Implemented  
**Date:** 2026-05-16  
**Deciders:** Afsin, Claude  
**Prerequisites:** ADR-0005 (Self-Healing Replanning), ADR-0010 (Tiered Failure Resilience), ADR-0012 (Synaptic Bridge)

---

## Context

The Council of High Intelligence analysis (`project_summary.md`) identified Cambrian's primary engineering gap:

> *"Cambrian's DAG cannot self-correct mid-execution. If an agent produces a bad output at Step 3 of a 7-step plan, Steps 4–7 continue to execute on top of it."*

ADR-0005 adds intra-step self-healing (retry with workspace rollback). ADR-0010 adds inter-step fallback and plan-level replan triggered by hard errors (agent returns an `error`). Neither addresses **silent coherence failures**: a step that completes successfully but produces output semantically incompatible with the original intent — wrong format, partial answer, off-topic content. These failures propagate silently through all downstream steps before the user sees them.

The biological analogue is **re-entrant processing** in the prefrontal cortex: before committing the result of a perceptual step to working memory and dispatching it to downstream areas, the cortex runs a brief coherence check against the original intent. If the check fails, it does not fire downstream signals — it re-routes to error correction.

This ADR adds that gate.

---

## Decision

### 1. Schema: Two New Fields on `domain.Step`

```go
type Step struct {
    Query            string  `json:"query"`
    DependsOn        []int   `json:"depends_on,omitempty"`
    IsThought        bool    `json:"is_thought,omitempty"`
    MaxEnergy        float64 `json:"max_energy,omitempty"`
    RecommendedModel string  `json:"recommended_model,omitempty"`
    CheckpointAfter  bool    `json:"checkpoint_after,omitempty"`  // ADR-0013
    CheckpointQuery  string  `json:"checkpoint_query,omitempty"` // ADR-0013
}
```

- **`CheckpointAfter`** — if `true`, the coordinator runs a semantic coherence check after the step result is merged into `masterContext`, before dispatching successor steps.
- **`CheckpointQuery`** — optional custom coherence question supplied by the Planner. If empty, `DAGExecutor` uses a standard fallback template.

The fields are `omitempty` — all existing plans and agent configs are unaffected.

### 2. New Error Type: `SemanticCheckpointError`

Added to `internal/domain/errors.go` alongside `BudgetExceededError`:

```go
type SemanticCheckpointError struct {
    StepIndex      int
    Assessment     string // full Thought step output, contains "REPLAN_SIGNAL"
    OriginalResult string // step output that failed the coherence check
}

func (e *SemanticCheckpointError) Error() string {
    return fmt.Sprintf("semantic checkpoint failed at step %d: %s", e.StepIndex, e.Assessment)
}
```

### 3. `runCheckpoint` — Private Method on `DAGExecutor`

The checkpoint dispatches a Thought step via the existing `ThoughtFn` hook. It does **not** call `EnqueueVerification` — a checkpoint is a planning-layer integrity check, not a quality sample for the Trust system.

```go
func (d *DAGExecutor) runCheckpoint(
    ctx context.Context,
    stepIndex int,
    planID string,
    step domain.Step,
    masterContext map[string]string,
) (assessment string, incoherent bool) {
    prompt := step.CheckpointQuery
    if prompt == "" {
        prompt = fmt.Sprintf(
            "[REENTRY_CHECK] Original intent: %q\nResult received: %q\n"+
                "Is this result coherent and sufficient to proceed? "+
                "If not, explain why and respond with REPLAN_SIGNAL.",
            step.Query,
            masterContext[fmt.Sprintf("step_%d_result", stepIndex)],
        )
    }
    resp, err := d.ThoughtFn(ctx, stepIndex, &domain.Handoff{
        Payload: &domain.Payload{Data: []byte(prompt)},
        Context: masterContext,
    })
    if err == nil && resp != nil && resp.Payload != nil {
        assessment = string(resp.Payload.Data)
    }
    if d.EventWriter != nil {
        _ = d.EventWriter.WriteTaskEvent(domain.TaskEvent{
            TaskID:  fmt.Sprintf("checkpoint-%d-%s", stepIndex, planID),
            AgentID: "System_Checkpoint",
        })
    }
    incoherent = strings.Contains(assessment, "REPLAN_SIGNAL")
    return
}
```

**Incoherence signal:** `REPLAN_SIGNAL` is a sentinel token the Thought step emits when its assessment is negative — consistent with the existing `REPLAN_SIGNAL` convention used by the Hermes loop (ADR-0005).

### 4. Coordinator Integration — H1 Pattern

The gate is inserted in the coordinator result loop after `CheckpointStore.SaveCheckpoint` and before `dispatch()`. It mirrors the `BudgetExceededError` control flow exactly: same mutex dance, same `errorPause` flag, same ReplanHandler wakeup path.

```go
// After CheckpointStore.SaveCheckpoint (existing, line ~614)

if step.CheckpointAfter && d.ThoughtFn != nil {
    assessment, incoherent := d.runCheckpoint(ctx, r.index, planID, step, masterContext)
    masterContext[fmt.Sprintf("step_%d_checkpoint", r.index)] = assessment
    if incoherent && firstErr == nil {
        firstErr = &domain.SemanticCheckpointError{
            StepIndex:      r.index,
            Assessment:     assessment,
            OriginalResult: masterContext[fmt.Sprintf("step_%d_result", r.index)],
        }
        failedStepIdx = r.index
        if d.ReplanHandler != nil && d.MaxReplanAttempts != 0 {
            d.pausedMu.Lock()
            d.paused = true
            d.errorPause = true
            d.pausedMu.Unlock()
        }
    }
}

if firstErr == nil {
    dispatch() // dispatch() only fires when gate passes
}
```

**DAG Immutability is preserved:** The gate does not modify the plan. It sets `firstErr` and `errorPause`, which triggers the existing ReplanHandler hot-swap mechanism. The original plan remains frozen.

### 5. Planner System Prompt — `CHECKPOINT STEPS` Section

Added after the existing `THOUGHT STEPS` section in `GetExecutionPlan`:

```
CHECKPOINT STEPS:
- After any step whose output gates irreversible or costly downstream work, set "checkpoint_after": true.
- Optionally supply "checkpoint_query" with a specific coherence question for that step. If omitted, the runtime generates a default template.
- Typical triggers: file writes, external API calls, format-transforming steps, any step that feeds 3 or more dependent steps.
- Example:
  {"query": "Convert CSV to JSON schema", "depends_on": [0], "checkpoint_after": true,
   "checkpoint_query": "Is the output valid JSON schema compatible with the downstream validator?"}
```

### 6. ReplanHandler — `CHECKPOINT FAILURE` Branch

`PlannerReplanHandler.Replan` already has a typed-error dispatch for `BudgetExceededError`. `SemanticCheckpointError` adds a parallel branch using the same `errors.As` pattern:

```go
var checkpointErr *domain.SemanticCheckpointError
if errors.As(err, &checkpointErr) {
    originalCheckpointQuery := ""
    if failedStep < len(originalPlan.Steps) {
        originalCheckpointQuery = originalPlan.Steps[failedStep].CheckpointQuery
    }
    checkpointConstraint = fmt.Sprintf(`
CHECKPOINT FAILURE:
- Step %d completed successfully but its output was assessed as incoherent before
  downstream steps were dispatched.
- Checkpoint assessment: %s
- Original coherence question: %q
- Do NOT simply retry the failed step. Diagnose why the output failed the coherence
  check and redesign the approach for the remaining work.

`, checkpointErr.StepIndex, checkpointErr.Assessment, originalCheckpointQuery)
}
```

The `checkpointConstraint` string is injected into the repair prompt in the same `%s` slot used by `modelConstraint`.

---

## Rejected Alternatives

| Alternative | Reason Rejected |
|-------------|-----------------|
| In-step coherence check (inside `executeStep`) | Violates DAG Immutability — the step cannot alter the plan it belongs to |
| Separate `CheckpointAgent` in the registry | Breaks hexagonal separation — awareness concern wired into metabolism |
| Hard-coded step types that always checkpoint | Violates Zero-Hardcode Rule — Planner must decide which steps are critical |
| Calling `EnqueueVerification` from `runCheckpoint` | Checkpoint is a planning integrity check, not a quality sample; would pollute TrustScore with synthetic data |

---

## Implementation File Map

| File | Change |
|------|--------|
| `internal/domain/errors.go` | Add `SemanticCheckpointError` |
| `internal/domain/plan.go` | Add `CheckpointAfter bool`, `CheckpointQuery string` to `Step` |
| `internal/metabolism/executer/dag_executor.go` | Add `runCheckpoint` method; H1 gate in coordinator result loop |
| `internal/awareness/planner.go` | Add `CHECKPOINT STEPS` section to system prompt |
| `internal/awareness/replan_handler.go` | Add `CHECKPOINT FAILURE` branch (`errors.As` dispatch) |

---

## Relationship to GWT

In Global Workspace Theory, **re-entrant signalling** is the mechanism by which the workspace verifies that a broadcast result matches the originating intent before committing it. ADR-0013 implements this as a lightweight Thought step that checks coherence out-of-band, between the result merge and the successor dispatch — structurally identical to GWT's re-entrant loop at the broadcast boundary.
