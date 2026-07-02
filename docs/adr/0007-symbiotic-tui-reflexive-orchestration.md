# ADR-0007: Symbiotic TUI & Reflexive Orchestration

**Status:** Implemented

**Date:** 2026-05-07

**Context:** v0.4.0-Alpha, Deep Kernel Migration

---

## Summary

We decided to transform Cambrian from a background gRPC server into a **unified single binary** that runs the Substrate kernel in a background goroutine and a Bubble Tea TUI shell on the main thread. The TUI communicates with the kernel via a bi-directional `ChatStream` gRPC connection. We introduced a hard-coded **Reflexive Path** intent triage layer that routes user inputs to either direct memory retrieval, planner-based task orchestration, or real-time HITL intervention — explicitly carving out an exception to the Zero-Hardcode Rule for shell-level utilities.

## Why This Decision Was Hard

### 1. Violation of a Core Invariant

The [Zero-Hardcode Rule](../../CONTEXT.md) states: "Routing decisions must never be expressed as Go `if-else` or `switch` statements." The Reflexive Path is exactly that — a hard-coded table mapping intent categories to system modes. We accepted this deviation because:

- The TUI and Reflexive Path function as the **OS shell**, not the agentic substrate.
- Shell utilities are governed by standard software logic; the intelligence layer (agent-to-task routing) remains zero-hardcode.
- A future LLM-based intent classifier would add latency to every keystroke with no meaningful gain for three unambiguous categories.

### 2. Concurrency Model Change

The DAG Executor was designed around **cancel-on-first-error** (`context.WithCancel`). Adding HITL pause requires a `sync.Cond` or atomic pause flag that all goroutines check at safe checkpoints. This changes the fundamental contract from "only cancellation stops progress" to "cancellation OR pause stops progress."

### 3. Protocol Break

Replacing the unary `Execute(Handoff) → Handoff` RPC with `ChatStream(stream Handoff) returns (stream SymbiosisEvent)` is a breaking change. All existing SDK integrations must migrate. We accepted this because the unary model cannot support real-time telemetry, thought streaming, or mid-process intervention.

## Decision

### A. Unified Single Binary

`cmd/orchestrator/main.go` boots both:
1. Substrate gRPC server (background goroutine)
2. Bubble Tea TUI loop (main thread)

A `--headless` flag disables the TUI for remote deployments.

### B. Bi-directional ChatStream Protocol

```protobuf
service Orchestrator {
  rpc ChatStream(stream Handoff) returns (stream SymbiosisEvent);
}
```

- **Upstream** (`Handoff`): User commands, approval signals, intervention text.
- **Downstream** (`SymbiosisEvent`): A `oneof` envelope carrying:
  - `ThoughtChunk` → Cortex pane
  - `ProgressUpdate` → Execution pane (progress bars)
  - `AgentLog` → Execution pane (structured logs)
  - `InterventionRequest` → Status pane (amber alert)
  - `StatusUpdate` → Status pane

The `oneof` pattern provides type-safe multiplexing without parsing overhead. The TUI's `Update()` loop uses a type-switch to route packets to the correct component.

### C. Reflexive Path (Intent Triage)

A hard-coded triage layer in `server.go` intercepts inputs before they reach the Planner:

| Intent | Trigger | Path |
|--------|---------|------|
| Direct Retrieval | Question about system state (e.g., "Status of LTM?") | Bypass Planner → `QueryMemory` → Direct Response |
| Task Orchestration | Action verb + object (e.g., "Build and test code") | Trigger Planner → DAGExecutor |
| HITL Intervention | Override keywords (e.g., "Stop", "Change param") | Signal DAGExecutor → Homeostatic Pause |

**Rationale:** The Reflexive Path is the OS shell, not the agentic substrate. The Zero-Hardcode Rule governs agent-to-task routing (which agent executes which step), not user-to-system-mode routing (whether to query memory or plan a task).

### D. StepType Enum on Step

The `Step` struct gains a `Type` enum:

```go
type StepType string

const (
	// StepTypeAction is a standard agentic task (requires Auction/Gatekeeper).
	StepTypeAction StepType = "action"
	
	// StepTypeThought represents a JIT reasoning step (Cortex-only).
	StepTypeThought StepType = "thought"
	
	// StepTypeIntervention triggers a Homeostatic Pause (Amber Mode).
	StepTypeIntervention StepType = "intervention"
)
const (
    StepTypeAction StepType      // Normal agent execution step
    StepTypeThought                      // JIT reasoning (bypasses Auctioneer)
    StepTypeIntervention                 // HITL approval gate (pauses DAG)
)

type Step struct {
    Type          StepType `json:"type"`
    RequiredTools []string `json:"required_tools,omitempty"`
    Query         string   `json:"query"`
    DependsOn     []int    `json:"depends_on,omitempty"`
    IsThought     bool     `json:"is_thought,omitempty"` // Deprecated: use Type
}
```

`StepTypeIntervention` triggers a **global DAG pause** using `sync.Cond`. All in-flight goroutines check the pause flag at safe checkpoints (before starting work, after completing work, before dispatching successors). The pause lasts until:
- User approves (`[Y]es`) → resume with original plan
- User edits (`[E]dit`) → Planner receives revision request, DAG is hot-swapped
- User cancels → DAG aborts with `context.Cancel`

### E. Differential Rendering

The TUI receives `SymbiosisEvent` state updates over the gRPC stream instead of polling. Each event targets a specific pane:
- **Cortex**: Thought chunks append to a collapsible tree; active thought expands, completed thoughts collapse to checkmarks.
- **Execution**: Agent logs stream into a scrollback buffer; progress updates render as `lipgloss` progress bars keyed by `agent_id`.
- **Status**: Intervention requests trigger amber mode; system status shows connection health and queue depth.

## Consequences

### Positive

- **Real-time symbiosis**: User sees agent reasoning and execution progress as it happens, not after completion.
- **Safe destructive actions**: `StepTypeIntervention` prevents accidental `rm -rf` without adding friction to safe operations.
- **Mid-process correction**: Ctrl+I allows users to override the Planner without restarting the entire task.
- **Single binary deployment**: No separate frontend server; one binary runs the full Agent OS.

### Negative

- **Breaking protocol change**: All SDK clients must migrate from `Execute()` unary to `ChatStream()` bidirectional. Proto freeze (Issue #016) is violated — this ADR supersedes it.
- **Backpressure risk**: High-volume agent logs can overwhelm the TUI. We must implement bounded buffers (256-event ring per pane) with `slog.Warn` when dropping old telemetry.
- **Pause complexity**: `sync.Cond` adds a second cancellation path to the DAG Executor. Developers must reason about both `ctx.Done()` and pause flags.
- **Hard-coded triage maintenance**: New intent categories require code changes, not prompt updates.

## Rejected Alternatives

### 1. LLM-Based Intent Classifier

We considered a lightweight LLM call to classify intent instead of hard-coded rules. Rejected because:
- Adds 50-200ms latency to every keystroke.
- Three categories (Direct Retrieval, Task Orchestration, HITL) are unambiguous — an LLM would be overkill.
- The shell is a utility layer; intelligence belongs in the substrate.

### 2. Separate TUI Binary

We considered a standalone `cmd/tui/` binary that connects to the Substrate via gRPC. Rejected because:
- Requires users to run two processes.
- Local loopback adds unnecessary latency for a single-user edge runtime.
- The unified binary model aligns with the "Agent OS" metaphor — the shell and kernel are inseparable.

### 3. Unary Execute + Polling for Telemetry

We considered keeping the unary `Execute` RPC and adding a separate `SubscribeTelemetry` stream. Rejected because:
- Chronological ordering between user commands and system responses is lost across two streams.
- The TUI must correlate request IDs across RPCs, adding client-side complexity.
- Single-stream `ChatStream` preserves causality naturally.

## Related Documents

- [CONTEXT.md §Zero-Hardcode Rule](../../CONTEXT.md) — Core invariant with explicit shell-layer exception
- [Plan: SymbioticInterfaceImplementation.md](../../plans/4-%20SymbioticInterfaceImplementation.md) — Detailed implementation plan
- [ADR-0002](../adr/0002-dag-pause-mechanism.md) *(planned)* — Detailed design of `sync.Cond` pause integration in DAGExecutor

## Glossary

- **SymbiosisEvent**: The Protobuf `oneof` envelope streaming from Substrate to TUI. Contains thought chunks, progress updates, agent logs, and intervention requests.
- **Reflexive Path**: The hard-coded intent triage layer that routes user input to Direct Retrieval, Task Orchestration, or HITL Intervention.
- **Homeostatic Pause**: The DAG-wide pause triggered by `StepTypeIntervention` or Ctrl+I, resolved by user approval, edit, or cancellation.
- **Differential Rendering**: TUI update strategy that modifies specific component models instead of full-screen redraws.
