# ADR-0033: Daemon Agent Architecture

**Status:** Implemented
**Date:** 2026-06-01
**Status History:** Proposed (grilling 2026-06-02) → Implemented (daemon-per-stream model shipped;
`SpawnDaemon`/`StopDaemon` on AgentManager, consumed by the premium ReactiveEngine via the seam).
**Author:** Afsin
**Replaces:** Hardcoded daemon spawning (composition root only)

---

## Context

Cambrian currently supports three agent traits (`TraitCognitive`, `TraitTool`, `TraitModel`) and three runtimes (`RuntimeA2A`, `RuntimePython`, `RuntimeBinary`). Daemon agents (long-running background processes that stream signals) are not a first-class concept. They are either:
- Hardcoded in the composition root at startup
- Spawned manually via `AgentManager.CallAgent` (which expects a task response, not a persistent stream)

REQ-ROUTER-002 needs daemon agents for the WATCH decision: "Spawn a gold tracker that polls an API every 5 seconds and streams price signals."

The core question: should Cambrian control daemon internals (what API it polls, how often, what format), or should the daemon be a black box?

---

## Decision

**Daemon agents are black-box third-party plugins.** Cambrian sees only their manifest. The daemon decides its own data sources, polling logic, and signal format.

### New Trait Constant — No New Runtime

```go
// AgentDefinition.Trait
const (
    TraitCognitive AgentTrait = iota // standard LLM agent
    TraitTool                        // deterministic script/binary
    TraitModel                       // LLM inference provider
    TraitDaemon                      // autonomous background agent (NEW)
)
```

**`RuntimeDaemon` is NOT introduced.** A daemon agent uses the same language runtime as any other agent (`RuntimePython` or `RuntimeBinary`). The boot path difference is driven entirely by `TraitDaemon`:

```go
// in AgentManager.buildAgentCmd — no new runtime constant needed
if def.Trait == TraitDaemon {
    cmd = append(cmd, "--daemon-mode", "--stream-id", streamID)
}
```

`--daemon-mode` tells the agent process to skip the task-response handshake and immediately open a persistent `SignalStream` connection. `--stream-id` tells it which stream ID to include in every signal it emits. The runtime (`python` or `binary`) determines how to invoke the executable — unchanged.

A daemon agent manifest declares `trait: "daemon"` and `runtime: "python"` or `runtime: "binary"`. There is no `runtime: "daemon"`.

### The Black-Box Contract

```json
{
  "gold_tracker": {
    "trait": "daemon",
    "runtime": "python",
    "capabilities": ["commodity_tracking", "price_monitoring"],
    "input_schema": {
      "properties": {
        "interval_seconds": {"type": "integer", "default": 60}
      }
    },
    "output_schema": {
      "properties": {
        "price": {"type": "number"},
        "currency": {"type": "string"}
      }
    }
  }
}
```

Cambrian knows:
- **Capabilities** — used for Router matching ("which daemon can track gold?")
- **InputSchema** — used for parameter extraction and validation (`santhosh-tekuri/jsonschema/v6`)
- **OutputSchema** — used for condition evaluation ("what fields are available in signal payload?")

Cambrian does NOT know how the daemon gets its data, what libraries it imports, or its internal state machine.

---

## Router Matching

When the Router classifies a WATCH (or CHAT) decision, it finds the right daemon via two paths:

**Path 1 — User names the agent directly:**
`"Watch using gold_tracker"` → direct `AgentRegistry.Get("gold_tracker")` lookup, validate `Trait == TraitDaemon`, proceed. No matching algorithm.

**Path 2 — User describes intent only:**
ANN semantic search against embedded daemon descriptions in pgvector. `TraitDaemon` agents use the **same `InterviewWorker` fast-path as `TraitTool` agents**: at registration time, `agent.Description` is embedded (no LLM scenario generation) and stored as `DocTypeAgentProfile` in pgvector. The Router embeds the user's WATCH/CHAT intent and performs ANN search against daemon descriptions.

```go
// InterviewWorker fast-path — one-line extension
if def.Trait == TraitTool || def.Trait == TraitDaemon {
    // embed Description, skip scenario generation
    embedding = embedder.Embed(def.Description)
    saveAgentProfile(def.ID, embedding)
    return
}
```

If multiple daemons match above threshold: `DecisionClarification` presented to user.
If zero match: `DecisionClarification` asking user to register a daemon or rephrase.

**Disambiguation:** specialized capability preferred over generic by ANN similarity score. No hardcoded preference rules.

---

## `StreamID` Semantics

`WatchSource.StreamID` = **agent ID** (e.g., `"gold_tracker"`). It is stable across daemon crashes and restarts. Any instance of `gold_tracker` emits signals under `stream_id: "gold_tracker"`. The daemon reads `--stream-id` from CLI args and includes it in every signal.

`StreamID` is NOT an instance UUID. Using instance UUIDs would silently orphan WatchConfigs on every crash — the WatchConfig would reference a stream ID that no longer exists.

---

## Daemon Lifecycle — Reference Counted

Daemon lifetime is **reference-counted by WatchConfig**. `AgentManager` maintains `map[streamID]int` ref counts:

| Operation | Effect |
|---|---|
| `RegisterWatch` (first for this `stream_id`) | ref count = 1 → `SpawnDaemon` |
| `RegisterWatch` (subsequent for same `stream_id`) | ref count++ → daemon already running |
| `DeleteWatch` | ref count-- → if 0: `StopDaemon` |
| `SetWatchActive(false)` | pauses evaluation; ref count unchanged; daemon stays running |
| `SetWatchActive(true)` | resumes evaluation; daemon already running |

A daemon with zero active WatchConfigs does not exist. There is no separate `StopDaemon` gRPC RPC — daemon lifecycle is fully managed through WatchConfig CRUD.

---

## `DaemonSpawner` — Implemented on `AgentManager`

```go
type DaemonSpawner interface {
    SpawnDaemon(ctx context.Context, agentID string, params map[string]any) (instanceID string, err error)
    StopDaemon(instanceID string) error
    ListRunningDaemons() []DaemonInstance
}
```

**Implementation on `AgentManager`** (not a new `DaemonManager` package):
- `SpawnDaemon` = `bootAgent` with `--daemon-mode` + `--stream-id` injected; instance stored with `mode: Daemon`
- `StopDaemon` = `EvictAgent` by instance ID
- `ListRunningDaemons` = filter existing instance map by `mode == Daemon`

`DaemonSpawner` is a consumer-side interface — `kernel/provider_premium.go` narrows `AgentManager` to it when wiring `DaemonRestartManager`.

---

## Daemon Params Persistence

`WatchConfig` gains a `DaemonParams map[string]any` field:

```go
type WatchConfig struct {
    ID            string
    Name          string
    Description   string
    Source        WatchSource
    Condition     string
    ConditionType string        // "deterministic" | "pattern" | "llm" | "always"
    Action        WatchAction
    Active        bool
    ResponseMode  string        // "" | "sync" — see §ResponseMode
    DaemonParams  map[string]any // populated only on first RegisterWatch for this stream_id
    MaxConcurrentPlans int      // default 1; for start_plan action only
}
```

`DaemonParams` is populated **only on the first `RegisterWatch` for a given `stream_id`**. Subsequent `RegisterWatch` calls for the same `stream_id` must supply empty `DaemonParams` (daemon already running) or params that exactly match the existing ones — mismatch is a validation error.

`DaemonRestartManager` reads `DaemonParams` from BBolt to restart crashed daemons with the correct configuration. See `REQ-DAEMON-RESTART-POLICY.md`.

---

## Parameter Injection

`SpawnDaemon` serializes `DaemonParams` to JSON and passes it via CLI arg, consistent with existing `buildAgentCmd` pattern:

```go
jsonParams, _ := json.Marshal(params)
cmd := exec.Command(agentPath,
    "--socket", sockPath,
    "--substrate-socket", substrateAddr,
    "--daemon-params", string(jsonParams),
    "--stream-id", streamID,
)
```

**Why CLI args:** consistent with existing boot path; no race condition (params available before `main()` runs); non-secret parameters (polling interval, endpoint URL) are acceptable in `ps` output.

**JSON Schema validation:** `InputSchema` on the manifest is validated against `DaemonParams` at `RegisterWatch` time using `santhosh-tekuri/jsonschema/v6` — the codebase-wide JSON Schema validation library. Required fields missing → validation error returned to caller.

---

## WatchConfig Fan-out Registry

The in-memory registry is `map[streamID][]*WatchConfig` — **not** `map[streamID]*WatchConfig`. Multiple WatchConfigs can reference the same `stream_id`. A signal arriving on `"gold_tracker"` is evaluated against all registered WatchConfigs for that stream concurrently, each in their own worker pool slot.

Per-WatchConfig de-duplication (`atomic.Pointer[Signal]`) and concurrency limits apply independently — three WatchConfigs on the same stream each hold their own slot.

`RegisterWatch` appends to the slice. `DeleteWatch` removes by `WatchConfig.ID`. `SetWatchActive` gates a single config without affecting siblings.

---

## CHAT as WATCH Unification

**Conversation daemons are WATCH daemons.** A conversation daemon watches a stream of user messages (`stream_id: "conv:{convID}"`) and reacts to each one. The architectural model is identical:

| WATCH (gold tracker) | CHAT (conversation daemon) |
|---|---|
| `stream_id: "gold_tracker"` | `stream_id: "conv:{convID}"` |
| `condition: "price > 5000"` | `condition: "always"` |
| `action: dispatch_agent → analyst` | `action: dispatch_agent → conversation_daemon` |
| `response_mode: ""` (async) | `response_mode: "sync"` |

`StartConversation` = `RegisterWatch` with `ConditionType: "always"` and `ResponseMode: "sync"`.
`SendTurn` = signal arriving on the conversation stream.
`EndConversation` = `DeleteWatch` → ref count 0 → daemon stopped.

See `REQ-CHATBOT-001` for the full chatbot engine design built on this unification.

---

## Crash Recovery

When a daemon process exits unexpectedly, `AgentManager` detects the exit via `cmd.Wait()`:
1. Publishes `DaemonCrashedEvent{AgentID, StreamID}` to `domain.EventBus`
2. Emits `TelemetryObserver.OnDaemonCrashed(agentID)`
3. Marks the daemon instance as `Dead` in the registry
4. Marks the stream as `"unavailable"` in the ReactiveEngine (WatchConfigs skip evaluation)

Automatic restart is handled by `DaemonRestartManager` subscribing to `DaemonCrashedEvent`. See `REQ-DAEMON-RESTART-POLICY.md`.

---

## `ConditionType: "always"` — New Evaluator

A new condition type for WatchConfigs that should evaluate every signal unconditionally (CHAT conversations, unconditional monitoring):

```go
// "always" — skips evaluation, always executes action
// Avoids unnecessary expression parsing overhead for high-frequency streams
```

`ConditionType` values: `"deterministic"` | `"pattern"` | `"llm"` | `"always"`.

---

## Consequences

### Good
- **Any third party can write a daemon.** No Cambrian code changes needed. IoT sensors, stock price scrapers, webhook listeners — all use the same manifest contract.
- **Zero-Hardcode preserved.** No signal-type-to-action mapping in Go code. User owns the logic.
- **Single trait, not two axes.** `TraitDaemon` drives boot behavior; runtime (`python`/`binary`) drives execution environment. No redundant `RuntimeDaemon` constant.
- **CHAT falls out of WATCH.** No separate ConversationEngine system needed. One rule engine, many use cases.
- **ANN discovery without Interview.** `InterviewWorker` fast-path gives daemon agents semantic discoverability with zero LLM profiling overhead.

### Bad
- **Parameter extraction requires JSON Schema parsing.** `santhosh-tekuri/jsonschema/v6` is a new dependency — acceptable given its codebase-wide utility (A2A card validation, WatchConfig payload validation).
- **Ref-count correctness.** A bug in `RegisterWatch`/`DeleteWatch` ref counting can leave orphaned daemons (leak) or prematurely kill shared daemons. The ref count map must be guarded by `sync.Mutex` in `AgentManager`.
- **Crash recovery is deferred.** `AgentManager` detects crashes and publishes events; automatic restart requires `REQ-DAEMON-RESTART-POLICY.md` to be implemented.

### Neutral
- **User confusion on multi-daemon disambiguation.** If multiple daemons match the same intent, the Router presents `DecisionClarification`. This is a UX concern, not a technical one.

---

## Related

- REQ-ROUTER-002 (full requirement document)
- ADR-0031 (Universal Input Router — classification layer)
- ADR-0032 (ReactiveEngine — rule evaluation layer)
- ADR-0001 (Trait classification — precedent for static bidder exception)
- REQ-DAEMON-RESTART-POLICY.md (deferred: automatic crash recovery)
- REQ-DAEMON-DAG-PIPELINES.md (future: persistent streaming agent topologies)
- REQ-CHATBOT-001 (chatbot engine built on WATCH/CHAT unification)
