# ADR 0001 — DAG-Based Parallel Execution for ExecutionPlan Steps

**Status:** Implemented  
**Date:** 2026-04-30  
**Deciders:** Afsin

---

## Context

`Server.Execute` iterated over `ExecutionPlan.Steps` sequentially, passing each step's output as the next step's `Payload.Data`. Independent steps were forced to wait for unrelated predecessors, making the sequential loop the primary latency bottleneck on the critical path (noted in CONTEXT.md §3 and §5).

The `Step` struct carried no dependency metadata — only `Agent` and `Query`.

---

## Decision

Replace the sequential loop with a DAG-based parallel executor governed by the following rules:

### 1. Dependency expression

`Step` gains a `DependsOn []int` field. The LLM Planner produces these indices as part of the JSON plan. Topological ordering is a mechanical Go algorithm; the *routing intelligence* (which steps depend on which) remains in the LLM — the Zero-Hardcode Rule is preserved.

### 2. Cycle detection and retry

`DAGExecutor` runs Kahn's topological sort synchronously after plan generation, before any goroutine is launched. A cycle causes one retry: the Planner is re-invoked with the cycle description included in the prompt so the LLM can reason about its error. A second malformed plan fails hard and returns an error to the caller.

### 3. State accumulation

Each goroutine receives a **snapshot copy** of the accumulated context map at dispatch time (constructed after all predecessor goroutines have merged). Goroutines do not share a live context pointer; there is no mutex contention during execution.

Step outputs are merged by the coordinator after goroutine return:
- Primary output → `"step_{i}_result"` in the master context
- All `resp.Context` keys → namespaced as `"step_{i}_{key}"` to prevent collision between parallel branches

`Payload.Data` for each dispatched step is set to the step's `Query` field. Predecessor outputs are available to the agent via the context snapshot.

### 4. Error propagation

Cancel-on-first-error via `context.WithCancel`. `sync.WaitGroup.Wait()` is called unconditionally after cancellation to drain all in-flight goroutines before returning. This prevents goroutine leaks regardless of which branch failed.

### 5. Timeout formula

Per-step timeout: `Timeout = (bid_latency * multiplier) + base_buffer`

`bid_latency` is taken from `AgentProposal.Latency` (the winning agent's own estimate). `multiplier` and `base_buffer` are fields in `configs/config.json`. A total plan timeout wraps the entire `DAGExecutor.Execute` call as the hard ceiling.

### 6. Architectural placement

- `internal/substrate/dag_executor.go` — DAG traversal, cycle detection, goroutine dispatch, context snapshot, merge, cancellation
- `internal/metabolism/executer.go` — unchanged single-step unit (Gatekeeper → Auction → `Manager.CallAgent`); called by `DAGExecutor` per goroutine
- `Server.Execute` stays thin: memory fetch → plan → `DAGExecutor.Execute` → return

---

## Alternatives Considered

| Concern | Alternative Rejected | Reason |
|---|---|---|
| Dependency edges | Infer from `Query` text at runtime | Fragile; routing logic in Go violates Zero-Hardcode Rule |
| Context sharing | `sync.RWMutex` on shared map | Snapshot eliminates contention during execution; cleaner goroutine isolation |
| Cycle failure | Let goroutines time out | Wastes latency; partial execution already happened |
| Payload for fan-in | Embed all predecessor outputs in `Payload.Data` | Breaks agents that expect a simple task string; Context is the designed accumulation envelope |
| DAG placement | `internal/metabolism/` | Metabolism owns per-node lifecycle; traversal strategy is a Substrate concern |

---

## Consequences

- `domain.Step` is a breaking change to the LLM prompt contract — the Planner system prompt must be updated to instruct the LLM to emit `depends_on` indices.
- Agents that read only `Payload.Data` and ignore `Context` will not see predecessor outputs in fan-in steps. This is a known capability gap in those agents, not a substrate defect.
- The `context.Background()` → `context.WithTimeout` audit (CONTEXT.md §5) is partially addressed by the per-step timeout formula; a separate pass is still needed for the total plan timeout wiring.
