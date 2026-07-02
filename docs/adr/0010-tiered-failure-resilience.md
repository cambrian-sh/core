# ADR 0010 — Tiered Failure Resilience & DAG Replanning

**Status:** Implemented  
**Date:** 2026-05-12  
**Deciders:** Afsin, OpenCode  
**Prerequisites:** ADR-0005 (Self-Healing Execution), ADR-0009 (Dispatcher & Neural Signal Layer)  

---

## Context

The current `DAGExecutor` enforces cancel-on-first-error: any step failure (even after `SelfHealer` exhausts its 3 intra-step retries) aborts the entire plan and discards partial results. This wastes expensive agent work, ignores ranked runner-up candidates from the Gatekeeper, and contradicts the Hermes replanning philosophy.

`ADR-0005` added intra-step SelfHealing (retry with workspace rollback). This ADR adds the two remaining tiers: **inter-step fallback** (try runner-up auction candidates) and **plan-level replan** (pause the DAG, call the Planner with error context, hot-swap the remaining plan, resume execution).

---

## Decision

### 1. Three-Tier Failure Model

| Tier | Trigger | Action | Locus |
|---|---|---|---|
| **Tier 1 — Intra-step** | Agent returns error | SelfHealer retries up to 3× with workspace rollback | `server.go` stepFn wrapper |
| **Tier 2 — Inter-step fallback** | Tier 1 exhausted | Try runner-up candidates (#2, #3) from Gatekeeper score list; inject `_fallback_reason` | Extended SelfHealer in `server.go` |
| **Tier 3 — Plan replan** | Tier 2 exhausted | Pause DAG → call `ReplanHandler` → hot-swap plan → resume | `DAGExecutor` pause/resume/hot-swap |
| **Final — Abort** | Tier 3 exhausted or max replans exceeded | Return `PartialPlanError` with accumulated context | `DAGExecutor` |

**Reflexive bypass:** Rejected adding an `IsReflexive` field to `domain.Step`. Deterministic safety steps are already handled by `TraitTool` agents, which bypass the Auctioneer's dynamic bidding and return `Confidence=1.0` instantly. The Planner does not need to pre-mark steps; the Gatekeeper naturally routes deterministic tasks to Tool-Agents.

### 2. Partial Result Contract — `PartialPlanError`

Instead of breaking `DAGExecutor.Execute`'s signature (which would cascade through `Server.Execute`, all tests, and the TUI), we introduce a backward-compatible wrapper:

```go
type PartialPlanError struct {
    FailedStep  int
    LastError   error
    Context     map[string]string // all step_{i}_result keys accumulated so far
    ReplanCount int
}
```

`DAGExecutor.Execute` returns `PartialPlanError` as the error value when the plan does not complete. Callers that only care about total failure can treat it as a normal error. Callers that want partial results (TUI, gRPC response builder) use `errors.As` to extract the context — consistent with the existing `PlanValidationError` pattern.

On success: `Execute` returns `(masterContext, nil)` as before.  
On partial failure: `Execute` returns `(nil, &PartialPlanError{...})`.

### 3. Inter-Step Fallback in SelfHealer

The fallback loop lives in `server.go`'s `stepFn`, extended from the existing `SelfHealer` wrapper:

```
1. SelfHealer wraps Auctioneer.Execute (Tier 1: up to 3 retries)
2. If Tier 1 exhausts:
   a. Check if Gatekeeper returned runner-up candidates
   b. For each runner-up (max 3), in rank order:
      - Skip if runner-up Confidence < FallbackConfidenceThreshold (default 0.4)
      - Inject `_fallback_reason` into Handoff.Context
      - Attempt execution via Auctioneer.callAgent with runner-up agent
      - If same AgentID, try a different Instance (phenotype) if available
      - On success: continue plan
   c. If all runners-up fail → propagate to Tier 3
```

**Instance isolation:** If the runner-up has the same `AgentID` as the failed winner, the Auctioneer attempts a different `Instance` (phenotype) if one is available. This handles phenotype-specific failures (deadlocks, socket errors) without penalizing the genotype. If no alternate instance exists, the fallback skips to the next candidate.

**Fallback confidence threshold:** If the #2 candidate's Confidence is < 40% of the winner's, skip fallback and proceed directly to replan. This avoids wasting time on clearly inferior alternatives.

### 4. DAGExecutor Pause/Resume/Hot-Swap

`DAGExecutor` gains three new fields:

```go
type DAGExecutor struct {
    // ... existing fields ...
    paused     bool
    pauseCond  *sync.Cond
    replanCount int
}
```

**States:**
- **Running** — default; `dispatch()` launches goroutines normally
- **Paused** — `paused = true`; no new dispatch; in-flight goroutines finish current work; `pauseCond.Wait()` blocks the coordinator
- **Resuming** — `HotSwap(newPlan)` called; new plan injected; `paused = false`; `pauseCond.Broadcast()` unblocks coordinator; dispatch continues from next topological position
- **Cancelled** — hard abort; `cancel()` fires; `wg.Wait()` drains

**Hot-swap rules:**
- New plan's step indices are remapped to continue from `failedStep`'s position (reuses existing `hotswap.go` remapping logic)
- `masterContext` preserved in full
- Steps already completed are marked in `completed` map and never re-dispatched
- Replan counter increments; if it exceeds `MaxReplanAttempts` (default 2), DAG aborts with `PartialPlanError`

**Signal-aware pause:** When the DAG enters `Paused` state for replanning, the Watcher is notified via `DAGExecutor`'s `EventSink`. The Watcher suppresses new `Inspiration` signals for the same error context, preventing duplicate Planner calls. No Inhibition signal is sent to Daemon Observers — they continue operating; only the Watcher's deduplication logic changes.

### 5. Context Checkpointing (Lazy Persistence)

An in-memory ring buffer (`map[planID][]ContextCheckpoint`) stores snapshots of `masterContext` after each successful step merge. Each checkpoint contains:

```go
type ContextCheckpoint struct {
    StepIndex      int
    Context        map[string]string
    Timestamp      time.Time
}
```

- **No disk I/O on hot path.** Checkpoints are held in memory only.
- **Flush to BBolt** occurs only when the DAG enters `Paused` state (replanning or user intervention).
- **Recovery:** On Substrate crash and restart, `BootstrapStorage` checks for unflushed checkpoints in BBolt and reconstructs the `masterContext` if a plan was mid-execution.
- **Last Known Good:** Since `masterContext` is only merged after a step succeeds, the checkpoint *before* a failed step is automatically the "last known good" context. No additional snapshot logic is needed — this is satisfied by the existing `cloneMap` semantics in `dag_executor.go:183`.

### 6. ReplanHandler Interface

```go
// ReplanHandler is called by DAGExecutor when a step exhausts all
// intra-step retries and inter-step fallback candidates.
type ReplanHandler interface {
    Replan(
        ctx context.Context,
        failedStep int,
        err error,
        partialContext map[string]string,
        originalPlan *domain.ExecutionPlan,
    ) (*domain.ExecutionPlan, error)
}
```

`PlannerReplanHandler` (in `internal/awareness/`) wraps the Planner with a specialised prompt:

```
REPLANNING REQUEST

Original request: {user_input}
Failed step: {step_index} — {step_query}
Error: {error_message}
Failed agent: {agent_id} (SourceHash: {hash})
Merit data: TrustScore={trust}, SuccessRate={rate}, Latency={lat}ms
Partial results so far:
- step_0_result: ...

Please produce a revised plan for the REMAINING work.
Constraints:
- Do NOT re-include steps that already succeeded.
- Avoid the tool/agent that failed: {failed_agent_id}
- Consider using alternative tools: {runner_up_tools}
- The new plan's step 0 corresponds to the original plan's step {failed_step}.
```

**Dry-run verification:** The first step of the replanned plan is validated by a `TraitTool` Validator (if available) before execution. If the Validator detects the same faulty parameters that caused the original failure, the system triggers HITL intervention immediately instead of waiting for the second replan attempt.

### 7. Config Additions

Three new fields in `ExecutionConfig`:

```go
MaxReplanAttempts          int   // default 2
FallbackEnabled            bool  // default true
FallbackConfidenceThreshold float64 // default 0.4 (40% of winner's confidence)
MaxPartialContextBytes     int   // default 51200 (50KB cap for Planner prompt)
```

### 8. Integration with Existing Systems

| System | Interaction |
|---|---|
| **SelfHealer** | Extended with fallback loop. No changes to retry classification or workspace rollback. |
| **Gatekeeper** | `ScoredCandidate` list already produced; no changes needed. |
| **Auctioneer** | `Execute` gains a `callAgentWithCandidate` helper for direct runner-up execution. `ConductAuction` unchanged. |
| **Planner** | Gains `Replan` method and replan prompt template. Normal planning unchanged. |
| **DAGExecutor** | Major change: pause/resume/hot-swap. Cancel-on-first-error becomes final fallback. |
| **TUI** | Gains `ReplanningState` rendering (Phase 5, deferred to ADR-0001 follow-up). |
| **ProfileAggregator** | Gains `replan_count` metric per agent. |
| **Watcher** | Suppresses duplicate Inspirations during replanning. |

---

## Considered Options

| Concern | Alternative Rejected | Reason |
|---|---|---|
| Partial results signature | Break `Execute` to return `Result` struct | Would cascade through Server.Execute, TUI, gRPC response, and all tests — too invasive for the benefit |
| Reflexive bypass | Add `IsReflexive bool` to `domain.Step` | Redundant with `TraitTool`; violates Zero-Hardcode by having Planner pre-select agent types |
| Checkpointing | Write to BBolt after every step | Synchronous disk I/O on critical path; 10-step plan = 10 disk writes |
| Checkpointing | No checkpointing | Loses durability guarantee; crash during replan loses all work |
| Fallback location | Inside `Auctioneer.Execute` | Would make Auctioneer responsible for retry logic, blurring the boundary between auction and execution |
| Fallback location | Inside `DAGExecutor` dispatch loop | Would require DAGExecutor to understand agent candidates, violating its role as pure DAG traverser |
| Last Known Good | Additional explicit backup field | Unnecessary — existing `cloneMap` snapshot semantics already preserve clean context |
| Dry-run verification | Skip verification, trust Planner | Risk of replan loops; Validator check prevents repeated faults |

---

## Consequences

- **`PartialPlanError`** is a new error type. Callers that `errors.As` on gRPC errors must handle it to extract partial context. Callers that don't will see a normal error — backward-compatible.
- **`DAGExecutor.Execute`** now has `pauseCond` and `HotSwap` — additional complexity in the coordinator loop, but the `sync.Cond` pattern is well-established in the codebase (PauseController already uses it).
- **`SelfHealer`** gains runner-up iteration and confidence threshold checks. The retry loop becomes more complex but remains localised in `server.go`.
- **BBolt checkpoint bucket** is a new persistent structure. `BootstrapStorage` must check it on startup for crash recovery.
- **Planner prompt size** is bounded by `MaxPartialContextBytes`. If partial context exceeds 50KB, older step results are truncated before injection.
- **Max replan ceiling** (`MaxReplanAttempts=2`) bounds LLM cost. Each replan consumes one Planner call.
- **TraitTool inference** means the Planner cannot explicitly mark a step as "reflexive." If a step requires deterministic safety logic, the Planner must describe it in natural language, and the Gatekeeper will match a Tool-Agent. This is consistent with the Zero-Hardcode Rule.

---

## Code Integration Points

| Path | Responsibility |
|---|---|
| `internal/substrate/dag_executor.go` | Pause/resume/hot-swap; `PartialPlanError`; lazy checkpoint flush |
| `internal/substrate/replan.go` | `ReplanHandler` interface definition |
| `internal/substrate/server.go` | Extended SelfHealer fallback loop; wire `ReplanHandler` into `DAGExecutor` |
| `internal/metabolism/auctioneer.go` | `callAgentWithCandidate` helper for runner-up execution |
| `internal/awareness/planner.go` | `Replan` method + replan prompt template + dry-run Validator hook |
| `internal/config/config.go` | `MaxReplanAttempts`, `FallbackEnabled`, `FallbackConfidenceThreshold`, `MaxPartialContextBytes` |
| `internal/storage/bbolt_adapter.go` | Checkpoint bucket (`"checkpoints"`) for lazy persistence |
| `internal/service/memory_agent.go` | `IngestNegativeEdge` call on ultimate replan failure |
| `internal/substrate/watcher.go` | Suppress duplicate Inspirations during replan |

---

## Success Metrics

- **Autonomous Recovery Rate:** >60% of step failures resolved without user intervention (fallback or replan).
- **Partial Utilisation:** 100% of `Execute` calls return partial context, even on failure.
- **Replan Cost:** Average additional latency per replan < 5 seconds (local LLM).
- **False Replan:** <5% of replans fail again on the same step (tracked via `replan_count` in TaskEvent).
