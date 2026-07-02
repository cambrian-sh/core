# ADR 0005 — Self-Healing Execution and Hermes Replanning

**Status:** Implemented  
**Date:** 2026-05-06  
**Deciders:** Afsin  
**Source Plans:** `plans/2.1- SelfHealing-Replanning-Opencode-Hermes.md`, `plans/2.1.1.1 - OpenCode/`, `plans/2.1.1.2 - Hermes/`

---

## Context

The current `DAGExecutor` operates on a cancel-on-first-error model — any agent execution failure cancels the entire plan and propagates a raw error to the caller. This is explicitly flagged as a critical gap in CONTEXT.md §5. There is no retry, no recovery, and no mechanism to distinguish a transient infrastructure failure from a genuine logic error in the agent's output.

Two external projects were evaluated as capability donors:

- **OpenCode** — git-backed filesystem snapshotting (track / patch / restore), PTY management, and file-editing tools written in TypeScript.
- **Hermes** — XML-based reasoning harness (`<thought>/<call>/<response>`), SQLite session state, structured error classification with 17 `FailoverReason` categories, and a multi-turn agent loop written in Python.

The integration philosophy is **Fork & Wrap** — extract minimal logic (DNA) from each source, port or reuse it inside Cambrian's existing module boundaries, and discard the rest.

**Note:** The Wasm sandbox approach was cancelled on 2026-05-06. Agents remain Python/binary gRPC processes for all future development.

---

## Decision

### 1. Architectural Locus — Substrate-Level SelfHealer

Self-healing lives at the Substrate layer. A `SelfHealer` struct in `internal/substrate/harness/self_healer.go` wraps the `StepFunc` closure in `server.go` before it is passed to `DAGExecutor.Execute`. The DAGExecutor goroutine for each step retries up to three times via the wrapped function before returning `HealingExhaustedError`. Agents are completely unaware of the retry — they receive successive `Handoff` calls with error context appended.

This placement is chosen over SDK-level healing because:
- Healing must be uniform across all agent runtimes (Python, binary).
- The Substrate owns `masterContext` and the timing of step dispatch.
- SDK-level healing is invisible to the Merit system and cannot be instrumented by the Verifier.

### 2. Rollback Scope — masterContext + Agent Workspace (Scope 2)

Two scopes are rolled back on a failed step:

**a. masterContext** — free by construction. The DAGExecutor coordinator never merges a failed step's output (the `continue` on first error already skips the merge). No explicit rollback code is needed.

**b. Agent workspace (`def.Dir`)** — a git-backed Copy-on-Write snapshot of the agent's working directory is taken before execution and restored on failure. This is required because agents in Cambrian are development-capable processes that manage folders and projects.

**Explicit constraint:** External side effects (writes to databases, outbound API calls made during a failed attempt) are **not rollback-safe** by any snapshot mechanism. Callers must design agents that defer external commits until the step is known to have succeeded, or accept that external side effects from failed attempts may persist.

### 3. Snapshot Lifecycle — Owned by `CallAgent`

`AgentManager.CallAgent` in `internal/metabolism/executer.go` owns the full snapshot lifecycle:
- Calls `harness.Snapshot(def.Dir)` immediately before invoking the agent, obtaining a `snapshotHash`.
- Stores the restore handle keyed by `(agentID, taskID)`.
- The `SelfHealer`, on receiving a failure, calls `AgentManager.Restore(agentID, taskID)` before dispatching the next attempt.

On a retry, the auction may select a different winning agent. The SelfHealer restores the original winner's workspace (to undo partial writes) before allowing a fresh auction to run.

`CallAgent` is the correct owner because it is the only layer that has both `def.Dir` and the moment immediately before agent execution. The existing TRUTH LAYER pattern (pre/post PWD and ENV diffing) in `executer.go` is the precedent for this placement.

### 4. XML Harness Placement

The Hermes XML harness (`<thought>/<call>/<response>`) lives in two places, serving different purposes:

**a. Python SDK (`cambrian-sdk-python/hermes_loop.py`)** — the agent's internal reasoning loop. The gRPC boundary carries only the final resolved result in `Payload.Data`. The Substrate never sees raw XML from agents. Neural Tracing is achieved by the agent SDK extracting the thought text and placing it in `resp.Context["_thought_trace"]` before returning; the Substrate treats this as a normal context key.

**b. Go Awareness layer (`internal/awareness/xml_parser.go`)** — a streaming XML parser used exclusively by the Planner's Hermes reasoning loop (see §5). This parser is not a Substrate or harness concern.

### 5. Planner Hermes Reasoning Loop

The Planner's `GetExecutionPlan` method in `internal/awareness/planner.go` is extended with a structured reasoning loop. Before emitting the final plan JSON, the LLM may produce `<thought>` blocks to reason step-by-step about agent capabilities, prior failures (Negative Edges), and Hippocampus templates. `xml_parser.go` strips thought blocks and extracts the final plan JSON.

**Scope for this ADR:** structured reasoning only — the LLM reasons over pre-injected context (Hippocampus prior plan, Negative Edges, agent manifests, memory context). The Planner does not dispatch real tool calls (memory queries, registry lookups) mid-loop. Real Planner tool calls are deferred to a future ADR.

The existing `planWithValidation` single-retry remains the fallback if the Hermes reasoning loop still produces an invalid plan. Maximum total LLM calls per `Execute` invocation is unchanged at 2 for validation purposes.

### 6. Retry Loop and Error Classification

**Fault Classification** (`internal/substrate/harness/classifier.go`) applies four rules in order:

| Rule | Signal | Classification |
|---|---|---|
| 1 | `codes.DeadlineExceeded` or `codes.Unavailable` | System Error |
| 2 | `NO_WINNER` from Auctioneer | System Error |
| 3 | Agent-set `resp.Context["_error_type"] = "system"` | System Error |
| 4 | All other completed-agent errors | Logic Error |

**Retry strategy by classification:**

- **Logic Error** → inject `_heal_error` and `_heal_attempt` into the Handoff Context; restore agent workspace; re-dispatch to a fresh auction. The Planner is never re-invoked.
- **System Error** → restore agent workspace; re-dispatch identical Handoff to a fresh auction. No merit impact.

**Maximum 3 attempts per step.** A third failure triggers `HealingExhaustedError` (see §9).

### 7. Memory Barrier — Opt-In with Forced Sync on Healing Steps

The Memory Barrier remains opt-in. `IngestSync` fires when the agent sets `_kernel_sync=true` in `resp.Context`, or when a PWD/ENV mutation is detected. All other steps use `ProcessAndStoreAsync`.

**Addition:** The `SelfHealer` forces `IngestSync` on any step that enters the healing loop, regardless of the agent's `_kernel_sync` signal. The error context and failure pattern must be persisted to pgvector before the retry so the Negative Edge lookup (see §8) is queryable during re-planning.

Making the Memory Barrier mandatory on every step is rejected — it serializes the parallel DAG through I/O and violates the edge-latency requirement.

### 8. Negative Edges

When a step exhausts all 3 healing attempts, the failure pattern (error message + last `Payload.Data` output) is stored in pgvector as `doc_type="negative_edge"` with `importance_score=3`. The existing `MemoryAgent.FetchContext` retrieves these alongside regular memories and injects them into the Planner prompt before planning, enabling the LLM to avoid repeating known failure patterns.

**Decay:** Negative Edges are subject to the existing `MemoryWorker` forgetfulness mechanism — `importance_score=3` is below the consolidation threshold, and edges that are never retrieved again decay via low `access_count`. No new expiry mechanism is introduced.

**Zero-Hardcode compliance:** The Substrate stores and retrieves failure patterns as opaque strings. The LLM (Planner) decides what to do with them. No Go conditional branches on pattern content.

### 9. Homeostatic Pause — Typed `HealingExhaustedError`

When healing is exhausted (3 attempts failed) or a semantic loop is detected mid-retry, the `SelfHealer` returns a `HealingExhaustedError` carrying:

```go
type HealingExhaustedError struct {
    StepIndex    int
    AttemptCount int
    LastError    error
    LoopDetected bool
}
```

This propagates through `DAGExecutor` to `Server.Execute`, which returns it as a structured gRPC error with rich metadata. The caller (CLI, SDK, orchestrator) decides on the response — pause, alert, or retry the full plan. TUI integration (Cortex View) is deferred to a future ADR.

### 10. Semantic Loop Detection — Two-Tier, No Embeddings

Loop detection runs before each retry (attempt 2 and 3) and compares the current attempt against the previous:

**Tier 1 (always runs):** Exact string equality on the error message. A match flags a potential loop.

**Tier 2 (runs only on Tier 1 hit):** Normalized Levenshtein distance on `Payload.Data`, only if payload size < 8 KB:

```
delta = editDistance(prev_output, curr_output) / max(len(prev), len(curr))
```

If `delta < 0.05`, the loop is confirmed and `HealingExhaustedError` is returned immediately with `LoopDetected=true`, before the remaining retry budget is spent. If payload is empty or > 8 KB, a Tier 1 hit alone confirms the loop.

Cosine similarity (embedding-based) is rejected — it requires an Ollama round-trip on every retry on edge hardware. Exact error string equality is a stricter and cheaper signal for Cambrian's structured gRPC error space.

### 11. Merit Impact — Verifier-Only on Ultimate Failure

No TrustScore multiplier is applied during in-flight healing retries. A 0.7x multiplier during retries would penalize agents that recover successfully — a perverse incentive.

The Verifier Pool scores the final output after the step completes (success or ultimate failure). On ultimate failure, the Verifier scores the failed output naturally (low `quality_score`), which drives `TrustScore` down via the existing EWMA. No separate healing penalty is introduced.

### 12. Conditional Reasoning — Agent LLM Decides

The choice between full Hermes `<thought>` reasoning and direct response is the agent LLM's own decision at execution time. The Python SDK prompt template instructs the agent: *"For simple, deterministic operations you may skip the `<thought>` block and respond directly."* No new field is added to `domain.Step`, and the proto contract (frozen per ADR issue #016) is unchanged.

### 13. Neural Tracing — Observability Only

`resp.Context["_thought_trace"]` is ingested into pgvector as `doc_type="neural_trace"` with metadata `{trace_id, plan_id, step_index, agent_id, heal_attempt}`. `MemoryAgent.FetchContext` explicitly filters this doc_type — traces never appear in Planner prompts. They are queryable externally for debugging and a future TUI Cortex View.

Negative Edges (structured failure patterns, §8) are the correct mechanism for feeding execution experience back into planning. Raw thought traces are too verbose and execution-specific to serve as planning context.

### 14. Package Structure

**New files:**

```
internal/substrate/harness/
  self_healer.go      — StepFunc wrapper; retry loop (max 3); HealingExhaustedError
  classifier.go       — 4-rule LogicError|SystemError classifier
  fs_state.go         — git-backed CoW: Snapshot(dir) / Restore(dir, hash)
  loop_detector.go    — two-tier: string equality + bounded Levenshtein

internal/awareness/
  xml_parser.go       — streaming XML parser for Planner <thought> loop

cambrian-sdk-python/
  hermes_loop.py      — <thought>/<call>/<response> XML harness for agents
```

**Modified files:**

```
internal/metabolism/executer.go    — CallAgent: Snapshot before, Restore on retry signal
internal/awareness/planner.go      — GetExecutionPlan: Hermes reasoning loop via xml_parser
internal/substrate/server.go       — stepFn wrapped by SelfHealer
internal/service/memory_agent.go   — FetchContext: filter doc_type="neural_trace"
```

**Import graph (verified cycle-free):**

```
substrate        → metabolism          (existing)
substrate        → substrate/harness   (new)
metabolism       → substrate/harness   (new)
substrate/harness → domain             (only)
awareness        → awareness           (same package, xml_parser)
```

`internal/substrate/harness` imports nothing from `substrate` or `metabolism`.

---

## Alternatives Considered

| Concern | Alternative Rejected | Reason |
|---|---|---|
| Healing locus | SDK-level (inside Python agent) | Only covers Python runtime; Merit system cannot observe it; Substrate owns masterContext |
| Rollback scope | External side-effect rollback | Impossible by definition; documented as explicit constraint instead |
| Snapshot timing | Pre-auction snapshot of all candidates | Wasteful; snapshots N agents when 1 executes |
| Snapshot timing | Split Auctioneer into SelectWinner + ExecuteWinner | Invasive refactor with same outcome as CallAgent ownership |
| XML parser placement | Go parser in `internal/substrate/harness/` | Harness has no need for XML; parser belongs with its sole consumer (Planner) |
| Memory Barrier | Mandatory sync on every step | Serializes parallel DAG through I/O; violates edge-latency requirement |
| Merit penalty | 0.7x TrustScore multiplier per retry | Penalizes successful recovery; Verifier already handles quality discrimination |
| Negative Edge | Permanent storage (no decay) | Stale patterns poison the Planner indefinitely; MemoryWorker forgetfulness already handles decay |
| Re-plan on retry | Planner rewrites failing step query mid-DAG | Risks producing unvalidated tool requirements; Homeostat has no clean mid-execution recovery path |
| Loop detection | Cosine similarity (embedding-based) | Requires Ollama round-trip per retry on edge hardware; exact string equality is stricter and cheaper |
| Homeostatic Pause | Blocking channel / signal on Server | Same observable outcome with more plumbing; typed error is consistent with PlanValidationError pattern |
| Conditional reasoning | Planner tags steps with `reasoning_mode` field | Proto is frozen; LLM at execution time has richer context than Planner at plan time |
| Neural traces | Retrievable by Planner | Pollutes planning context with verbose monologue; Negative Edges are the correct planning-feedback mechanism |

---

## Consequences

- **`HealingExhaustedError`** replaces the raw cancel-on-first-error for recoverable step failures. Callers that `errors.As` on the gRPC error must handle this new type.
- **`CallAgent` signature change:** gains snapshot/restore hooks; `executer.go` takes a new dependency on `internal/substrate/harness`.
- **Agent workspace requires git init.** `fs_state.go` uses git as the backing store for CoW snapshots. Agent directories that are not git repositories will fall back to no-op snapshots (logged at WARN).
- **Python SDK gains `hermes_loop.py`.** Agents that adopt the Hermes loop gain structured reasoning and automatic thought tracing. Agents that do not adopt it continue to work unchanged — the `_thought_trace` key is optional.
- **`MemoryAgent.FetchContext` gains a doc_type filter.** The `neural_trace` type is explicitly excluded from planning context. Future doc types must be similarly registered.
- **Planner Hermes tool calls are deferred.** The structured reasoning loop (§5) is pre-injected-context only for this ADR. Dynamic tool dispatch from within planning (registry queries, memory lookups mid-reasoning) is a distinct future ADR.
- **Wasm branch in `CallAgent` is dead code** and will be removed in a follow-up cleanup issue.
