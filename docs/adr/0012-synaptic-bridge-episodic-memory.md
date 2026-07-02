# ADR-0012: The Synaptic Bridge — Episodic Memory & Session Management

**Status:** Implemented
**Date:** 2026-05-12
**Deciders:** System Architect (user), AI Agent

---

## Context

Cambrian's current architecture treats every `Server.Execute` call as a stateless transaction. The `DAGExecutor` generates an ephemeral `planID`, executes steps, and discards all context when it returns. The `Hippocampus` stores *procedural* memory (plan templates), but there is no *episodic* memory: the runtime cannot answer "What did we discuss yesterday?" or resume a conversation after a TUI crash.

This creates three critical gaps:

1. **No temporal continuity** — Users cannot resume work after disconnection.
2. **No self-reflective dataset** — The ConsolidatorAgent (autonomic memory consolidation) has no session narrative to analyze.
3. **PartialPlanError context is lost** — When a plan fails, the accumulated `masterContext` is returned to the caller but never persisted. A crash after `PartialPlanError` means total data loss.

The biological analogue is the **Hippocampus (Episodic Layer)**. This ADR defines how Cambrian acquires it.

---

## Decision

### 1. Session as Conversation Container

A `Session` is a persistent UUID-scoped conversation. It is **not** a single plan execution — it is a higher-order container that holds multiple plans over time.

- **Creation:** On first user message (implicit) or explicit `/new` command.
- **Status lifecycle:** `Active` → `Paused` (HITL / error replan) → `Dormant` (TUI disconnect) → `Completed` (manual `/done` or Circadian Rhythm).
- **Scope:** Each `Execute` call within a Session generates a new ephemeral `planID`, but all checkpoints and events are namespaced under the Session.

### 2. Unified Event Sourcing

A single `"events"` bucket in BBolt stores all events with a `Type` field. `TaskEvent` (Merit metrics) and `SessionEvent` (narrative/mood) are **views** on the same underlying log.

- **Key format:** `SessionID:Timestamp:Sequence` (ordered, lexicographic).
- **SessionEvent types:** `UserMessage`, `SystemThought`, `HITLIntervention`, `CriticalError`, `BudgetBreach`.
- **Checkpoint events:** Special `CheckpointSaved` events link `SessionID:PlanID:StepIndex` to the checkpoint hash.

**Rationale:** Two separate buckets (`task_events` + `session_events`) would require dual writes at every step and complicate the Consolidation Pipeline. A unified log is simpler, and projections (`ProfileAggregator`, `ConsolidatorAgent`) read filtered views.

### 3. Context Checkpoints: Backend Preserves All, TUI Shows Latest

After each successful DAG step, `DAGExecutor` asynchronously persists:
- `masterContext` snapshot
- The associated `ExecutionPlan` (for replay)
- Metadata: `SessionID`, `PlanID`, `StepIndex`, `Timestamp`

**Storage key:** `SessionID:PlanID:StepIndex` in the `"checkpoints"` BBolt bucket.

**Resume semantics:** The TUI defaults to the **latest** checkpoint (Option A UX). The backend preserves **all** checkpoints (Option B data model). Branching lets users select any historical checkpoint.

**Rationale:** The full checkpoint history is required for branching ("Go back to step 2 and try a different model"). But the TUI shouldn't overwhelm users with a timeline on every resume.

### 4. The Synaptic Watcher (Amygdala Observer)

A background worker in `MemoryStack` tails the unified event log. It is the system's **Amygdala** — deciding which signals are significant enough to move from Short-term Buffer (BBolt) to Long-term Memory (pgvector).

**Heuristic priority filter:**

| Event Type | Priority | Action |
|---|---|---|
| CriticalError | 10 | Immediate LTM Ingest: High-priority "Poisoned Memory" entry |
| HITLIntervention | 8 | Immediate LTM Ingest: Captures "User corrected a behavior" |
| UserMessage | 5 | Buffer: Wait for Consolidation unless keywords detected |
| ThoughtStep | 3 | Discard: Keep in BBolt for history, don't embed |

**Rationale:** Decouples event producers (DAGExecutor, ChatStream) from pgvector I/O. The observer sees the full stream and can detect cross-event patterns (e.g., `TaskEvent(Failure)` + `UserMessage("You always do this!")` → synthesized frustration memory).

### 5. Circadian Rhythm (Session Auto-Completion)

Dormant sessions auto-transition to `Completed` after `session_ttl_days` (default 7). Manual completion (`/done`) is also allowed. The transition triggers the Consolidation Pipeline.

**Biological analogue:** Sleep consolidation — chaotic daily events are compressed into structured wisdom during rest.

**Rationale:** If sessions never complete, BBolt grows unbounded and the Consolidation Pipeline never runs. A TTL honors the biological metaphor while bounding storage.

### 6. ConsolidatorAgent (Privileged System Agent)

The Consolidation Pipeline is executed by a **ConsolidatorAgent** — a `TraitCognitive` agent that serves the Kernel, not the user. It is the **Autonomic Nervous System** performing metabolic consolidation.

**Invocation:** Direct call from the Circadian Rhythm background goroutine (not via Auctioneer).

**Inputs:** Full SessionEvent log + latest checkpoint context.

**Outputs:**
- **Semantic:** Updated `AgentProfile` or new LTM documents (learnings from failures).
- **Procedural:** Optimized `ExecutionPlan` stored as a template in pgvector.
- **Cleanup:** Large checkpoints deleted; `SessionRecord.Summary` updated.

**Rationale:** Zero-Hardcode Rule compliant. The consolidation logic lives in the Awareness layer (LLM), not in Go conditionals. But because it serves the Kernel (not user tasks), it bypasses the Auctioneer — a Privileged System Agent.

### 7. Artifacts (CAS Vault + Semantic Map)

An Artifact is any non-ephemeral product generated by a Phenotype (Agent Instance).

**Creation:**
- **Agent-signaled:** The agent includes `ArtifactDescriptor` in its gRPC Handoff (`context["_artifacts"]`).
- **Future (Layer 2):** The DAGExecutor monitors the agent's sandbox for file writes and auto-registers Provisional Artifacts.

**Storage:**
- **The Vault (Filesystem):** Content-Addressable Storage at `data/vault/{hash}`. Deduplication: identical content → identical hash → stored once.
- **The Map (pgvector):** Artifact metadata (name, purpose, sessionID) + semantic summary embedded for retrieval.

**Reference:** SessionEvent of type `TaskCompleted` contains `ArtifactIDs []string`.

**Hash purpose:** Deduplication + integrity. If a resumed session's file hash has changed (manual user edit), it's treated as a Mutation and a new Artifact version is created.

**Rationale:** Separates payload (filesystem, CAS) from index (pgvector, semantic). Prevents "cognitive obesity" from redundant storage.

---

## Consequences

### What Becomes Easier

1. **Resume after crash** — TUI reconnects, loads latest checkpoint, continues from where it left off.
2. **Self-reflective awareness** — The ConsolidatorAgent has a full narrative dataset to analyze ("This error was made in the previous session; our budget is more constrained now").
3. **Mood-aware planning** — The Planner injects the last 3 SessionEvents (especially user reactions) as "Social Context" in its prompt.
4. **Branching experiments** — Users can fork a session at any historical checkpoint and try a different model or approach.
5. **Storage efficiency** — CAS deduplication + checkpoint cleanup prevent unbounded BBolt growth.

### What Becomes Harder

1. **Event log migration** — The existing `"task_events"` bucket must be merged into the unified `"events"` bucket. This is a one-time migration at startup.
2. **Checkpoint size** — Storing the full `ExecutionPlan` alongside every checkpoint doubles storage per step. Mitigation: plans are small JSON (queries + dependencies); the real payload is in `masterContext`.
3. **Circadian Rhythm daemon** — A new background goroutine must scan Dormant sessions daily. Failure modes: what if the ConsolidatorAgent fails during consolidation? → Retry with exponential backoff; if still failing, mark session as `Completed_Unconsolidated` and alert operator.
4. **Artifact garbage collection** — The CAS vault (`data/vault/`) accumulates orphaned files if no SessionEvent references them. A background sweeper must scan `ArtifactIDs` and delete unreferenced hashes.

### Rejected Alternatives

| Alternative | Why Rejected |
|---|---|
| Separate `task_events` and `session_events` buckets | Dual writes at every step; complicates Consolidation Pipeline |
| Session replaces `planID` entirely | Breaks existing Merit tracking (AgentProfile keys include SourceHash, not Session) |
| Auto-complete on idle timeout (N minutes) | Too aggressive; user might be reading output. Days-scale TTL is biologically appropriate |
| Hardcoded Consolidation Pipeline in Go | Violates Zero-Hardcode Rule; loses semantic summarization |
| Artifact content in BBolt | Bloats the DB; filesystem CAS is simpler and deduplicates |

---

## Implementation Notes

### New Domain Types (`internal/domain/`)

```go
type Session struct {
    ID        string        // UUID
    ParentID  string        // For branching
    Goal      string        // User-provided or LLM-generated summary
    Status    SessionStatus // Active / Paused / Dormant / Completed
    Summary   string        // Generated by ConsolidatorAgent
    CreatedAt time.Time
    UpdatedAt time.Time
}

type SessionStatus string

const (
    SessionActive    SessionStatus = "active"
    SessionPaused    SessionStatus = "paused"
    SessionDormant   SessionStatus = "dormant"
    SessionCompleted SessionStatus = "completed"
)

type SessionEvent struct {
    SessionID   string
    Type        SessionEventType
    Timestamp   time.Time
    Payload     string            // JSON blob
    ArtifactIDs []string
}

type SessionEventType string

const (
    EventUserMessage      SessionEventType = "user_message"
    EventSystemThought    SessionEventType = "system_thought"
    EventHITLIntervention SessionEventType = "hitl_intervention"
    EventCriticalError    SessionEventType = "critical_error"
    EventBudgetBreach     SessionEventType = "budget_breach"
    EventCheckpointSaved  SessionEventType = "checkpoint_saved"
)

type Artifact struct {
    Hash        string            // SHA-256 of content
    ContentType string            // MIME type
    SizeBytes   int64
    SessionID   string
    StepIndex   int
    Metadata    map[string]string // name, purpose, etc.
}
```

### New BBolt Buckets (`internal/storage/`)

- `"sessions"` → `SessionRecord` JSON
- `"events"` → Unified event log (replaces `"task_events"`)
- `"checkpoints"` → Already exists; extend key to `SessionID:PlanID:StepIndex`

### New Components

- `internal/substrate/session_manager.go` — Session lifecycle (create, pause, resume, complete)
- `internal/substrate/synaptic_watcher.go` — Event log tailing + heuristic ingestion
- `internal/substrate/circadian_rhythm.go` — Background daemon for Dormant → Completed transition
- `internal/awareness/consolidator_agent.go` — Privileged System Agent for consolidation

### Migration Path

1. **Phase 1 (Session Foundation):** Create `"sessions"` and `"events"` buckets. Write SessionEvent on every ChatStream message and DAG step. Checkpoint key extended to include SessionID. TUI shows SessionSelector on startup.
2. **Phase 2 (Resume):** HydrateSession loads latest checkpoint + plan. DAGExecutor replays from step N+1.
3. **Phase 3 (Consolidation):** Circadian Rhythm daemon + ConsolidatorAgent. Cleanup old checkpoints.
4. **Phase 4 (Branching):** Child Session creation with ParentID. UI for checkpoint selection.

---

## Related

- ADR-0001 (AgentTrait classification)
- ADR-0010 (Tiered Failure Resilience — PartialPlanError + CheckpointStore)
- ADR-0011 (Neuromodulator — Cost-Aware Routing — ModelMetrics in AgentProfile)
- CONTEXT.md §3 (Decision Matrix)
- CONTEXT.md §7 (Glossary: Session, SessionEvent, Synaptic Watcher, Circadian Rhythm, ConsolidatorAgent, Artifact)
