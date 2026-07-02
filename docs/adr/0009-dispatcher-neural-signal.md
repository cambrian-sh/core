# ADR-0009: Cambrian Dispatcher & Neural Signal Layer

## Status

Implemented (grilled 2026-05-11)

## Context

Cambrian currently operates as a **reactive** system: the user sends an Intent (via TUI or gRPC), the Planner generates a plan, the Auctioneer assigns agents, and the DAGExecutor runs it. Agents only speak when spoken to.

However, real biological systems are **proactive**: sensory neurons fire asynchronously when they detect changes, and the prefrontal cortex (Planner) decides whether to act. We need the same capability — agents that observe the world and signal the Substrate without waiting for a user command.

### Constraints

- **Zero-Hardcode Rule:** No Go `if-else` or `switch` maps signal types to actions. The Planner (LLM) must decide what to do with a signal.
- **Deep Kernel:** All signals pass through Substrate supervision; agents cannot trigger plans directly.
- **Windows 10 1803+:** UDS (AF_UNIX) requires this minimum version.
- **No backward compatibility:** Agent boot contract changes from TCP ports to UDS sockets.

## Decision

### 1. Genotype vs Phenotype

We distinguish between:
- **Genotype** (`AgentDefinition`, `AgentManifest`): Static identity — skill set, trait, merit score. Immutable during runtime.
- **Phenotype** (`Instance`): Runtime expression — a process, sandbox, or session spawned by the Substrate. Multiple Phenotypes can exist for one Genotype.

This allows the Substrate to spawn an Analyst (JIT), an Observer (Daemon), or 100 instances of the same agent as needed.

### 2. No `ScalingStrategy` on Genotype

Daemon mode is a **runtime property of the Phenotype**, not a static field on `AgentDefinition`. The Substrate decides at runtime: "I will keep one Instance of StockAgent in Daemon mode for observation." This keeps the Genotype minimal and lets the Substrate manage phenotype expression.

### 3. Signal is a Semantic Layer over Handoff

Instead of a new protobuf message, Signals reuse `pb.Handoff`:
- `SignalType` is carried in `Context["_signal_type"]`
- The Watcher only accepts Handoffs that carry a valid `SignalType`
- `SignalStream` is a separate gRPC endpoint from `ChatStream` to isolate telemetry traffic

This avoids new wire formats while keeping routing paths distinct.

### 4. Zero-Hardcode Signal Processing

The Watcher is a **thin pipe**:
```
signal → LTM context enrichment → unstructured prompt → Planner
```

No deterministic mapping from signal type to action exists in Go code. The Planner (LLM) decides whether the signal matters and what plan to generate.

### 5. UDS (Unix Domain Sockets) as Primary Transport

All local agent traffic moves from TCP to UDS:
- Each instance gets a unique socket path: `/tmp/cambrian_<agentID>_<instanceID>.sock`
- Agent boot contract changes: `--socket <path>` replaces `--port <port>`
- Substrate connects via `grpc.Dial("unix:"+path, ...)`
- Socket files are unlinked in `KillAllAgents`

**No TCP fallback.** This is a hard break. Every agent must be updated.

### 6. Instance Tracking with UUIDs

`AgentManager.ActiveAgents` changes from `map[string]*exec.Cmd` (agentID → process) to:
```go
map[string]*Instance  // key: instanceID (UUID)
```

With a secondary index:
```go
agentIndex map[string][]string  // agentID → []instanceID
```

Each instance tracks: `cmd`, `socketPath`, `mode` (JIT/Pool/Daemon), `authToken`.

### 7. Token-Based Signal Authentication

At instance boot, the Manager generates a short-lived token and passes it to the agent via `--auth-token`. The agent includes this token in gRPC metadata on every `SignalStream` message. The Watcher validates it against the Manager's instance registry.

### 8. Watcher Lives in `internal/substrate/`

The Watcher is part of the Substrate's "nervous system" — it receives external stimuli (signals) and triggers internal responses (planning). It belongs alongside `server.go` and `dag_executor.go`.

## Consequences

### Positive
- Cambrian becomes **proactive**, not just reactive — agents can trigger plans without user input
- UDS provides better isolation than TCP loopback — no port collisions, filesystem-level access control
- Genotype/Phenotype separation enables complex runtime scaling strategies (pools, daemons, JIT)
- Token auth prevents signal spoofing without mTLS certificate management overhead

### Negative
- **Hard break:** All agent code must update from `--port` to `--socket`
- **Windows 10 1803+ required** — no UDS on older Windows
- **Instance tracking refactor** touches `AgentManager`, `Auctioneer`, `DAGExecutor`, and all call sites
- **Token management** adds state to `AgentManager` that must be persisted across restarts

## Implementation Order

1. `internal/domain/agent.go` — no changes (Daemon is runtime, not static)
2. `api/proto/cambrian.proto` — add `SignalStream` RPC to `Orchestrator` service
3. `internal/metabolism/manager.go` — refactor `ActiveAgents` to `Instances` map + UUID index
4. `internal/metabolism/executer.go` — update `bootAgent` for UDS + token generation
5. `internal/substrate/watcher.go` (NEW) — SignalStream handler, LTM enrichment, Planner trigger
6. `internal/substrate/server.go` — add `Watcher` field, wire into gRPC server
7. `cmd/orchestrator/main.go` — construct Watcher in `bootstrapKernel`
8. All agent `.py` files — update arg parsing from `--port` to `--socket`

## References

- CONTEXT.md §1 (Core Philosophy) — biological properties table
- CONTEXT.md §3 (Zero-Hardcode Rule)
- CURRENT_CODEBASE_STATE.md §2.1 (substrate module status)
