# ADR 0004 — Deep Kernel Migration: Wasm Sandboxing, WASI-HTTP Interception, and Recursive Bidding

**Status:** Cancelled
**Date:** 2026-05-03
**Status History:** Proposed → **Cancelled**. The Wasm-sandboxing direction was abandoned;
its implementation slices live under `docs/issues/adr04-cancelled/`. (Self-healing/replanning
was taken up later by ADR-0005.)
**Deciders:** Afsin, Gemini CLI

---

## Context

Cambrian v0.3.0 used an `os/exec` model for agent execution. While this provided process-level isolation, it offered zero visibility into agent I/O (network/filesystem) without intrusive proxies. This "black box" behavior prevented the Substrate from performing essential "Kernel" duties: perfect observability, automated LTM ingestion of agent experiences, and centralized security enforcement.

Furthermore, critical architectural gaps remained:
1. **Internal Reasoning (Prefrontal Cortex):** The Planner acted as a scheduler but lacked a mechanism for middle-of-plan synthesis or logical inference between agent steps.
2. **Recursive Bidding:** Agents had no mechanism to satisfy inter-agent dependencies declared in `AgentProposal.Requirements`.
3. **Agent Monitor (The Watchdog):** No subsystem existed to audit low-level syscall behavior (WASI), enforce resource metabolism (CPU/RAM quotas), or detect behavioral drift.
4. **Atomic Memory Swap:** The daily memory consolidation worker suffered from a race hazard between `Ingest` and `Delete` operations.
5. **Human-in-the-Loop (HITL):** No robust mechanism existed to pause execution when uncertainty exceeded safe thresholds.

---

## Decision

Migrate the Substrate to a **Deep Kernel Approach** (v0.4.0) by implementing the following architectural shifts:

### 1. The Wall: Wasm Sandboxing & Capability Mapping
Replace `os/exec` with **WebAssembly (Wasm)** cells.
- **VFS Mapping:** Capabilities are strictly "injected" via WASI. Host file paths are mapped to isolated sandbox folders; unauthorized access attempts are blocked and logged.
- **Deterministic Snapshots:** To support Hot-Resume, the `WasmRuntime` shims non-deterministic WASI calls (time, entropy). During resumption, the host ensures temporal continuity, making the pause transparent to the agent.

### 2. The Lens: WASI-HTTP Interception & Kernel-Managed TLS
All outbound network traffic is intercepted at the WASI layer.
- **Plain-Text Visibility:** The Substrate sees request/response bodies in plain text before encryption/after decryption.
- **Hot-Client Pool:** To maintain low-latency, the Substrate manages a pool of persistent, keep-alive TLS connections.
- **Significance Filtering:** Intercepted traffic is passed through a **Sampling & Significance Filter**. Meaningful interactions are sent to the `MemoryAgent` for importance scoring and LTM ingestion.

### 3. Prefrontal Cortex: Virtual Thought Steps
The `ExecutionPlan` now supports **Thought Steps** — virtual nodes that do not trigger gRPC calls.
- **Inline JIT Reasoning:** The `DAGExecutor` dispatches Thought Steps as soon as their dependencies are met, prompting the LLM to perform synthesis or logical inference based on real-time predecessor outputs.
- **Observability:** These steps are logged under a `System_Thought` tag in `TaskEvent` for transparent debugging.

### 4. Recursive Bidding: Nested Sub-Auctions
When a winning agent declares `Requirements []string`, the Substrate triggers **Nested Sub-Auctions** before the winner's `Execute` call.
- **Namespaced Inlining:** Results are merged into the winner's context under `requirements.{name}.result` to prevent collision and ensure traceability.

### 5. The Watchdog: Syscall Auditing & Homeostasis
The `AgentMonitor` acts as the system's "Immune System."
- **Context-Aware Pain Scoring:** Behavioral violations (unauthorized domains, high-entropy payloads) increment a per-agent Pain Score. Weights and thresholds are dynamic: **Provisional** agents have stricter monitoring, while **High-Merit** agents have higher tolerance for transient anomalies.
- **Kill Switch & Rollback:** When the Pain Score exceeds a threshold, the Watchdog kills the cell and triggers a state rollback to the last stable BBolt snapshot.

### 6. Atomic Memory Swap: Shadow Buffer Swap
Memory consolidation now utilizes a **Shadow Buffer** strategy. Summaries are performed in a temporary space and committed to the main `documents` table within a single Postgres transaction (`BEGIN...COMMIT`).

### 7. Homeostatic Pause: State-Machine Persistence
If the `Uncertainties` score exceeds the threshold:
1. The `DAGExecutor` serializes the current state to a `PendingIntervention` bucket.
2. The `Execute` call returns a `PAUSED` status with a `planID`.
3. Execution can be resumed via a `Resume(planID)` RPC after human intervention.

---

## Alternatives Considered

| Component | Alternative Rejected | Reason |
|---|---|---|
| Execution Model | Containerization (Docker) | Too heavy for edge targets; millisecond cold-starts are required. |
| Reasoning | Sequential Synthesis | Blocks the DAG; prevents parallel "Thinking" while other agents work. |
| Monitor Trigger | Absolute Threshold | Too brittle; minor transient anomalies shouldn't always kill a task. |
| TLS Management | Agent-side TLS | Prevents Kernel-level observability and LTM ingestion of encrypted payloads. |
| Memory Swap | Global Write Lock | Blocks the system during consolidation; violates low-latency goals. |

---

## Consequences

- **Python SDK Update:** Standardizing on a Wasm-compiled Python build for all agents.
- **Watchdog Overhead:** Real-time syscall auditing adds a minor latency tax that must be compensated by the Hot-Client Pool.
- **Breaking Change:** Existing `os/exec` agents will require migration to the Wasm-compatible SDK.
