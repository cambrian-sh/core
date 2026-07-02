# ADR-0047: Operator Transport Plane — A Sequenced gRPC Surface for the Cambrian UI

**Status:** Proposed (2026-06-13) — design recorded and **grilled** (12-branch stress-test session); not implemented.
**Date:** 2026-06-13
**Author:** Afsin
**Depends on:** ADR-0030 (TUI deleted; `InMemoryEventBus` is the internal event substrate this plane consumes), ADR-0034/0035 (scope + kernel-derived write classification — the plane surfaces and never bypasses these; it reads at `ScopeSystem`), ADR-0039/0040 (kernel-owned tool registry + `ApprovalController`/`ApprovalHub` — the HITL and grant surfaces the plane controls and reuses), ADR-0042 (centralized LLM provider — the Cost/Health screen reflects broker state), ADR-0018 (`GenerateViaModelStream` managed proxy — the token-streaming tap), ADR-0012 (`Session`/`Checkpoint`/unified event log — the source of truth for persistent state), ADR-0032/0033 (premium reactive/daemon surfaces — capability-gated in the plane).
**Relates to:** Web UI PRD (`docs/requirements/UI/web-ui-prd.md`), requirements (`docs/requirements/UI/web-ui-requirements.md`), transport research (`docs/requirements/UI/Cambrian UI Transport Layer Architecture.md`). Supersedes the vestigial `SymbiosisEvent`/`ChatStream`/`SignalStream`-to-UI envelope built for the deleted TUI.

---

## Context

The Cambrian runtime has no human surface since the TUI was deleted (ADR-0030). The Web UI PRD requires a mission-control client — chat + operator console — that is **realtime by default** (ID-5, no manual refresh), **mutates only through the kernel's boundary** (ID-3), **reads only the kernel's data plane** (ID-9), is **role-gated server-side** (ID-7, Operator/Viewer), and is **deployable remotely** with a degraded connection as a first-class state (ID-10). The UI team has frozen **Tauri + Rust** for the client and **gRPC + Protocol Buffers** for transport.

Four facts about the current kernel shape this ADR:

1. **There is no event egress.** The network `Server` (`internal/substrate/network/server.go`) holds **no reference to the `EventBus`**. `ChatStream` is declared in `cambrian.proto` but **unimplemented**; `SignalStream` is the per-agent signal path, not a system-wide operator feed. The `SymbiosisEvent` oneof is leftover TUI scaffolding (it still references the retired `TraitTool` static bidder).
2. **The `EventBus` is a control-plane bus, not an observer feed.** `InMemoryEventBus` (`internal/domain/event_bus.go`) is **synchronous, in-process, unbuffered, with no sequence numbers and no replay**. Handlers run inside `Publish`, concurrently from multiple publisher goroutines. Hanging a network client off `Subscribe` would let a slow client block a publisher (e.g. stall an auction).
3. **Event coverage is incomplete.** The bus publishes 7 domain event types. The PRD §7.3 requires ~9 classes; **memory-written, HITL-raised, verifier-round, LLM-health, and audit are not domain events today.**
4. **There is no read source for ephemeral runtime state, and no operator-audit store.** In-flight plan state lives only in `DAGExecutor` goroutine memory (`inFlight` is a local counter; no plan registry). `SessionManager.ListSessions` exists; an `ExecutionControlHub`-style approval rendezvous (`ApprovalHub`) exists for HITL; but a queryable **operator-mutation audit log** does not (only `EgressAuditor` slog + tool arg/result hashes).

**Tauri + Rust collapses the browser-era transport problem.** The transport research evaluates gRPC-Web vs SSE vs WebSockets, `protobuf.js` decode cost, the HTTP/1.1 6-connection limit, and `BroadcastChannel` multi-tab election. **None apply here:** the Rust core holds a *native* gRPC client (`tonic`/HTTP2) straight to the kernel and bridges to the webview over Tauri IPC. Topology is two hops:

```
Go kernel ──gRPC/HTTP2 (tonic, 1 conn)──> Rust core (Tauri) ──Tauri IPC──> Webview (TS)
  [domain]                                  [the transport client]          [a projection]
```

No Envoy/gRPC-Web proxy; no V8 decode cost (prost in Rust); no multi-tab election (one webview, one core); a webview reload is a cheap local resync from the still-connected core, not a kernel reconnect. The research's resilience math (backoff+jitter), session/sequence/replay model, and state-projection discipline remain correct — they relocate from JS into the Rust core.

---

## Decision

Add an **Operator Transport Plane** — a primary (driving) gRPC adapter (`internal/substrate/operator/`) exposing a single **sequenced event feed**, point-in-time **snapshots**, and **idempotent audited commands**, fed by a new outbound domain port that decouples the synchronous `EventBus` from slow network clients. It is a **controller over the existing boundary**, never a back door.

### D1 — A separate `OperatorConsole` service

`cambrian.proto`'s `Orchestrator`/`AgentService` are the **agent-facing** contract, authenticated by `x-agent-id`. The operator plane is a different audience (human Operator/Viewer) with different auth. A new `api/proto/operator.proto` defines `service OperatorConsole`. The `SymbiosisEvent`/`ChatStream` envelope is **superseded, not extended** — overloading it risks an agent reaching an operator RPC or vice-versa.

### D2 — The outbound port `OperatorFeed` (the decoupling seam)

A new domain port `OperatorFeed` sits between the synchronous `EventBus` and the network stream. The plane subscribes to the `EventBus`, translates domain events into sequenced `OperatorEvent`s, and pushes them into a **bounded in-memory spool** (D9). `StreamEvents` drains the spool. **The synchronous bus is never blocked by a slow consumer**: a lagging client is forced to resync, never back-pressures the publisher. Domain stays import-pure; the spool/sequencer/projection live in the adapter.

### D3 — Missing events become first-class DomainEvents

`internal/domain/event.go` gains `MemoryWrittenEvent`, `HITLRaisedEvent`, `VerifierRoundEvent`, `LLMHealthEvent`, and `AuditEvent`. The producing organs publish them on the existing `EventBus`; the plane stays a **pure consumer**. (Chosen over synthesizing events inside the adapter, which would reintroduce the decorator/polling coupling the `EventBus` was built to remove.)

### D4 — Global monotonic sequence, one sequencer, one-timeline-one-publisher

`seq` is a **single global monotonic counter** across all event classes and all sessions (the resume cursor is one scalar, not a per-session vector — and "Plans in Flight across all sessions" is inherently cross-session). It is assigned by a single sequencer (`atomic.Uint64`) at the moment an `OperatorEvent` enters the spool — the one point that serializes the bus's concurrent publishers. Global `seq` is an **arrival order, not a causal order**: across independent timelines the interleaving is arbitrary (fine); *within* one timeline causal order holds **only because one publishing goroutine emits it in order**. This makes **"one logical timeline = one publishing goroutine"** an enforced constraint on every D3 producer (e.g. one plan's step events come from its single `DAGExecutor` goroutine).

### D5 — Tiered event fatness

Events carry a **self-contained delta — enough to fold into local state without a round-trip — but not whole entities**:
- **State-transition events** (plan/agent/verifier/lifecycle/HITL/audit/LLM-health): fat enough to render the change (the changed step's full record, the agent's new state+trust).
- **Token chunks:** thin and ephemeral (fragment + step ref); never replayed (D7/D9).
- **Large payloads** (memory-doc body, artifact): the event carries **CID + summary**; the webview pulls the body via the existing `ContentStore`/`GetContextNode` path (reuses the ADR-0022 push/pull discipline, no new fetch).

### D6 — Recovery: lower-bound seq + idempotent absolute-state (no sequencer lock)

`Snapshot` is **not** an atomic cut (it fans in over the in-memory projection, BBolt, and Postgres, which takes real time). Locking the sequencer across those reads would stall publishers — forbidden by D2. Instead:
- `as_of_seq` is captured at the **start** of the snapshot read (a lower bound), before any store is touched. The client resumes `StreamEvents` from `as_of_seq + 1`.
- Re-delivering events the snapshot already absorbed is **harmless because every event is an absolute-state assignment, not a delta** — re-applying is idempotent. **Gaps are impossible (lower bound); duplicates are harmless (idempotent fold).**
- This makes **"events carry absolute state, never increments" a hard rule** for every D3 producer (cost-so-far is `$0.42`, never `+$0.05`). Token chunks are the sole append-style class and are therefore **excluded from replay** (a reconnecting client resyncs accumulated step text from the snapshot/ContentStore).

### D7 — The plane owns live-plan state as a feed-fold projection

In-flight plan state has no kernel read source (fact 4). Rather than add a hot-path `PlanRegistry`, the plane **maintains its own projection by folding the feed**; "plans in flight" = un-terminated entries in that projection. This falls out of D5/D6 for free (events are already fat, absolute-state, foldable). Because the plane is an **in-process adapter sharing the kernel's lifetime**, there is no boot skew — the projection captures every plan from kernel start. **Caveat, stated plainly: in-flight plan visibility does not survive a kernel restart — by design, because the in-flight plans (goroutine memory) do not survive it either.** The projection is the authoritative source for *ephemeral runtime* state; persistent state comes from its stores (D8).

### D8 — `Snapshot` source map and scope

| Surface | Source | Nature |
|---|---|---|
| Plans in flight, active step, cost-so-far, elapsed | **plane feed-projection** (D7) | ephemeral |
| Sessions (list/state/checkpoints) | `SessionManager.ListSessions` + BBolt event log | persistent |
| Agents (genotype + live state + TrustScore) | agent registry + `ProfileStore` | persistent |
| System/LLM health | LLM-provider broker + projection | mixed |
| Capabilities + kernel/contract version | the plane (D14) | static |

The **resync snapshot contains only bounded live operational state** (the rows above). **Large/historical/searchable data — memory documents, the audit log, completed plans, verifier history — is never in the snapshot;** it is fetched via on-demand **paged read RPCs** with filters. A million-document LTM never bloats a reconnect (success criterion: status green in 5s).

### D9 — The spool: 120s time-primary window, shared ring, two-level backpressure

- **Window:** time-primary, `T ≈ 120s` (covers wifi blips, sleep/wake, webview reloads; under the research's 2–5 min TTL), with **event-count and byte ceilings** as memory safety caps, whichever binds first. All three are config, never constants.
- **One shared ring, per-client cursors** (D4's global seq makes a cursor a single scalar). No per-client rings.
- **Two-level backpressure, both → `RESYNC_REQUIRED`, never blocking the kernel:** *retention* (cursor falls behind the ring tail → resync; the ring always drops oldest) and *liveness* (a `StreamEvents` stream's bounded send buffer can't drain → evict that client to resync). One slow laptop cannot stall the feed for others.
- **Token chunks ride a separate live-only lane**, excluded from the ring (D5/D6) — a token storm cannot blow the byte cap and evict everyone.

### D10 — Resilience lives on the kernel↔core hop

Backoff+jitter (`base=1s, factor=2, cap=30s, jitter=±10%`, attempts capped ~2–5 min then a first-class "kernel unreachable" state) runs in the Rust core's `tonic` client. The webview↔core hop is in-process Tauri IPC and is **not** given network-grade resilience; a webview reload is a local resync because the core stays connected and holds live state (satisfies story 53 — drafts/in-flight/pending-approvals survive a refresh — by construction).

### D11 — Commands reach live executions via an `ExecutionControlHub`

Commands cannot be folded into a projection — they must hit the live goroutine. Generalize the existing `ApprovalHub` rendezvous into a **session-keyed `ExecutionControlHub`** holding **control handles, not state** (`{Pause, Resume, HotSwap, PauseController}`); the `DAGExecutor` registers at `Execute`-start, deregisters on completion; a missing session → clean "no live execution" error. (Distinct from the read-registry declined in D7: handles vs state.)
- **HITL resolution reuses `ApprovalHub` verbatim** (`ResolveHITL` = `SubmitApprovalDecision` under `OperatorConsole`), idempotent against the approval-request ID.
- **Inject mid-plan is a pipeline:** `Pause → ReplanHandler folds the injected instruction into a new ExecutionPlan → HotSwap(newPlan) → Resume`. The **Planner does the routing** — the NL correction passes through the awareness layer, never a Go branch (D16) — and it lands *inside* the running plan (UC-3), not queued behind it.
- **Operator-driven executions always wire a `PauseController`**, closing the CONTEXT known gap ("unary `Execute` HITL — PauseController only on the streaming path") *for the operator path*.

### D12 — Output streaming: step-level guaranteed, token-level best-effort

`AgentService.Execute` is unary — the kernel sees a step's *final* `Handoff`, not a token stream; the only token-level observation point is the `GenerateViaModelStream` managed proxy (ADR-0018). Three tiers:
- **Step-level (guaranteed):** started/running/finished/failed + final output, from the D3 `DAGExecutor` events. Every screen renders correctly on this floor alone.
- **Token-level (best-effort):** `TokenChunk` populated **only** for steps whose agent generates via the managed proxy, by a read-only tap on the existing metered stream.
- **Thought-level:** Planner reasoning where `Server.GenWrapper` already traces it.

The ADR states plainly: *token-by-token output is available only for managed-proxy generations; self-calling agents render at step granularity.* **Guaranteed token streaming for all agents is deferred post-V1**, but the design leaves the seam: the spool-excluded `TokenChunk` lane (D5/D9) is where a future streaming `AgentService.ExecuteStream` plugs in without reshaping the feed. (Agent response streaming is a planned future capability.)

### D13 — Security: `ScopeSystem` plane, RPC-gated roles, mandatory client auth

- **The plane operates at `ScopeSystem`** — above the per-agent scope plane, like the `ConsolidatorAgent`. The scope system isolates *agents from each other*, not *human operators from the system they operate*. Operator and Viewer **both see all data** (matches stories 49–50); the role distinction is about **mutation, not visibility**. No per-subscriber payload filtering and no PII masking on the feed in V1 (masking is for the LTM-storage path).
- **Role gate = RPC-access filter only** (a gRPC interceptor): Viewer gets `StreamEvents` + `Snapshot` + read RPCs and the *same* realtime data; every command RPC returns `PermissionDenied`. Enforced server-side; the UI merely reflects it.
- **Mandatory per-principal client authentication, always-on (not deferred), not even loopback-exempt.** TLS authenticates the *server* and encrypts the pipe — it does **not** authenticate the client; a random client can complete a TLS handshake. A separate credential gate is therefore required. A **single global shared secret is rejected** (it cannot attribute the audit `actor` or distinguish Operator/Viewer). V1: a `Login` RPC takes **username/password → short-lived token bound to `{principal, role}`**, resolved through an `OperatorIdentity` port; the Tauri core stores the token in the **OS keychain** and presents it on every call (stream-open included → `Unauthenticated` before any RPC). **`actor` is the resolved `principal_id`, never the request body** (mirrors the `x-agent-id` discipline). **TLS is additive**, mandatory on the remote core↔kernel hop, on top of the credential check.
- **Consequence, ratified: plane authN is the entire data-security boundary** — anyone who authenticates as Viewer reads all memory, all secrets ever in LTM, all audit history. There is no defense-in-depth below auth for the plane, by design. Scoped/partial viewers and the enterprise identity backend (OIDC/SSO/directory, behind the `OperatorIdentity` port) are deferred post-V1.

### D14 — OSS/premium: plane in OSS, premium surfaces capability-gated and excisable

- **`OperatorConsole` service + feed + spool + snapshot + commands + audit + auth live in OSS** (`internal/substrate/operator/`). An OSS kernel runs a complete mission-control for all OSS features.
- **Premium capabilities (Watch CRUD, daemon controls) are capability-gated through the existing `provider_oss.go`/`provider_premium.go` fork**, not a second plane. In OSS: premium command RPCs return `Unimplemented`; their event classes never publish (the reactive engine isn't running), so the feed omits them.
- **A capability handshake** (carried in the snapshot/subscribe, with kernel + contract version per D15) reports which surfaces this build supports; the UI **hides** unsupported screens (the structural analog of how Viewer hides mutations) — keeping ID-9/ID-11 honest.
- **Excisability invariant** (verified against the existing pattern — `Server` embeds `pb.UnimplementedOrchestratorServer`; `provider_oss.go` imports the OSS `internal/reactive` shim, never `internal/premium/`): deleting `internal/premium/`, all `//go:build premium` files, and `provider_premium.go` from the open-source repo leaves the default build compiling and running. The plane preserves this via five rules — (1) the server embeds `pb.UnimplementedOperatorConsoleServer`; (2) premium handlers live only in `//go:build premium` files; (3) no untagged/OSS file imports `internal/premium/` (premium reached through OSS-owned ports with nil/no-op defaults, like `ApprovalHub`); (4) premium wiring enters only via `provider_premium.go`; (5) `operator.proto` stays in OSS in full (defining ≠ implementing).

### D15 — Audit: a Postgres `operator_audit` store; audit write = idempotency dedup

- A new **Postgres `operator_audit`** table (not BBolt, not the event log): Postgres is already a dependency, gives indexed filtering (actor/target/type/time) + `COPY` export for free, and is a clean domain separation from BBolt's agent/session DTOs. Schema: `id, command_id (UNIQUE), ts, actor, role, action_type, target_type, target_id, before_json, after_json, reason, result`.
- **Before/after is captured by the command handler's own transactional view** (not a racy read-before/read-after in a decorator); the audit decorator only **persists + emits**.
- **The audit write IS the dedup:** D-contract's `command_id` is `UNIQUE`; a retried command (mid-reconnect replay) hits the constraint, returns the original result without re-applying. Idempotency and audit are one fact.
- **Write-then-emit:** persist the row, *then* emit the `AuditEvent` — so any client folding the event (or resnapshotting) always finds the durable row. Reads write nothing here, so "mutations loud, reads quiet" is structural.

### D16 — Zero-Hardcode boundary

Transport framing, sequencing, auth, dedup, and the control hub are deterministic — the same exception class as System Shell and the Reflexive Path (Omurilik). The plane **never routes tasks to agents**: forwarding a chat message or a mid-plan correction into a session is permitted because the Planner still does all routing (D11). No agent-selection `if`/`switch` may appear in this module.

---

## Contract standards (the wire)

One schema (`operator.proto`, owned by the kernel repo), generated to three targets via **`buf`** (`buf.gen.yaml`): Go (kernel, existing flow), Rust (`tonic`/`prost`, core), TS (webview, `ts-proto`/`tauri-specta`). **Generated bindings are committed** in all three. CI runs `buf lint` + **`buf breaking`** against main so a backward-incompatible operator-contract change fails the build. **The UI is a separate repo that vendors `operator.proto` pinned to a kernel version**; the capability handshake (D14) carries kernel + contract version, and the UI surfaces version skew as a first-class state (a newer kernel still serves an older pinned UI via `buf` backward-compat; a UI pinned ahead hits `Unimplemented` and degrades). No language ever hand-writes a message type.

```protobuf
service OperatorConsole {
  rpc Login(LoginRequest) returns (LoginResponse);                  // username/password → token{principal,role}
  rpc StreamEvents(SubscribeRequest) returns (stream OperatorEvent); // sequenced feed; SubscribeRequest{last_seq, filters}
  rpc Snapshot(SnapshotRequest) returns (SnapshotResponse);          // bounded live state, stamped as_of_seq + capabilities + version
  // paged read RPCs: QueryMemory / QueryAudit / ListCompletedPlans / ... (filtered, paginated; never in Snapshot)
  // commands (idempotent, audited; {command_id, actor-from-ctx, reason}):
  //   CreateSession / SendMessage / InjectCorrection / ResolveHITL / TagMemory /
  //   SetScope / SetToolGrant / RegisterSkill / RegisterMCP / TriggerConsolidation /
  //   [premium] RegisterWatch / SetWatchActive / ... → CommandAck
}

message OperatorEvent {
  uint64 seq = 1;                       // global monotonic; the resume cursor (D4)
  google.protobuf.Timestamp ts = 2;
  string session_id = 3;                // empty for system-global
  oneof payload {                       // absolute-state (D6); covers PRD §7.3
    PlanStateChanged plan = 10;  AgentStateChanged agent = 11;
    MemoryWritten memory = 12;   HITLRaised hitl = 13;
    VerifierRound verifier = 14; WatchFired watch = 15;        // premium
    LifecycleEvent lifecycle = 16; LLMHealthChanged llm = 17;
    AuditEntry audit = 18;       SystemStatus status = 19;
    TokenChunk token = 20;            // live-only lane; never spooled/replayed (D5/D9/D12)
  }
}
```

Conventions: cursor resume (`last_seq` → in-window replay or `RESYNC_REQUIRED`); idempotent commands (`command_id` UUID, deduped at the audit `UNIQUE`, D15); mandatory `{reason}` + context-derived `actor` on mutations; bounded spool with resync-over-blocking backpressure (D9).

---

## Module layout (hexagonal)

```
internal/substrate/operator/   ← NEW primary adapter (OSS)
  service.go    — OperatorConsole gRPC impl (embeds pb.UnimplementedOperatorConsoleServer)
  feed.go       — EventBus → sequencer → bounded spool → StreamEvents
  projection.go — feed-fold live-plan projection (D7)
  snapshot.go   — bounded live-state fan-in (D8)
  control.go    — ExecutionControlHub (session-keyed control handles, D11)
  audit.go      — Postgres operator_audit persist + emit decorator (D15)
  authz.go      — Login + OperatorIdentity port + role interceptor (D13)
  premium_watch.go  — //go:build premium  (Watch/daemon command handlers, D14)
  mapper.go     — domain ↔ pb.operator (the only translation point here)
internal/domain/operator_feed.go ← OperatorFeed port + new DomainEvents (D3) + OperatorIdentity port
api/proto/operator.proto         ← NEW contract (kernel-repo-owned, buf)
postgres: operator_audit table   ← NEW (D15)
```

`internal/domain` purity preserved; `internal/kernel` remains the only composition root and wires `EventBus`, the control hub, the identity provider, and (via the provider fork) premium surfaces into the plane.

---

## Build order

1. **Slice 1 (spine):** `OperatorFeed` + sequencer + spool + `StreamEvents`, bridging the **existing 7** domain events → watch one auction/plan live in the webview. Validates egress + sequencing + projection + resync end-to-end.
2. **Slice 2:** D3 events (HITL + audit first — the demo path needs them), `Snapshot` + cursor resume, the capability/version handshake.
3. **Slice 3:** command RPCs + `ExecutionControlHub` + `operator_audit` + `Login`/role interceptor; migrate `POST /v1/admin/*` behind a deprecated shim, then remove.
4. **Slice 4:** fan-out screens (MCP/skill registration, memory explorer, scope blast-radius, cost/health); premium Watch/daemon surfaces.

---

## Consequences

**Positive:** one realtime data plane and one mutation surface (ID-9/ID-3); the synchronous control bus is structurally protected from slow clients (D2/D9); gap-free recovery without locking the kernel (D6); webview reloads are free (D10); live-plan visibility needs no kernel hot-path structure (D7); one schema across three languages with a CI breaking-gate (contract); the OSS repo builds and runs full mission-control with the premium tree deleted (D14); mutation audit and idempotency are one durable fact (D15); Zero-Hardcode and hexagonal invariants hold (D16).

**Costs / risks:** D3 touches several producing organs (auction, verifier, memory, LLM provider, approval) to publish absolute-state events under the one-publisher rule (D4); the spool window is a tuning parameter (D9); the admin-HTTP→gRPC migration is a transitional double-surface until the shim is removed; **plane authN is the whole data-security boundary** (D13) — it raises the stakes on credential handling and on the deferred enterprise identity backend; token streaming is non-uniform until the future streaming `AgentService` lands (D12).

**Deferred (behind seams):** durable cross-restart event replay (V2 — the in-memory spool is V1); the enterprise identity backend + scoped/partial viewers (behind `OperatorIdentity`, D13); guaranteed per-agent token streaming via a streaming `AgentService.ExecuteStream` (the `TokenChunk` lane is the seam, D12); admin-HTTP→gRPC cutover ordering; concrete per-event-class latency budgets and the design-system component inventory (technical/UX docs).

---

## Amendment A1 (2026-06-15) — Inject-replan mechanism & TagMemory tag-mutation semantics

Recorded for issue 0047-20 (decided by the implementing engineer with the grilling context; open to ratification). Two decisions the producer wiring forced, both *under* ADR-0047 (still Proposed), not a new ADR.

### A1.1 — InjectCorrection is executor-owned (`ExecutionControls.Inject`)

The operator plane delivers only the **instruction string**; the live `DAGExecutor` owns the mechanism, because it (not the plane) holds the running `masterContext`, the completed-step set, and the plan machinery.

- **Mechanism:** `ExecutionControls` gains `Inject(instruction string) error`. The executor: coordinator-`Pause` → an optional **`InjectPlanner` seam** composes a new `ExecutionPlan` for the *remaining* work from `{instruction, current masterContext, current plan, completed steps}` (the Planner does the routing — Zero-Hardcode, D16) → `HotSwap(newPlan)` → resume. `ExecutionControls` does **not** expose `HotSwap` (the plane cannot sensibly build a domain plan); this supersedes the literal wording in 0047-22.
- **Completed-step preservation / DAG immutability:** the executed prefix is never rewritten — `applyReplannedPlan` merges the new plan onto the existing completed/dispatched state, so only the *future* changes. The invariant is preserved **per segment**: each contiguous run between hot-swaps equals its approved plan, and the operator's inject *is* the approval of the next segment.
- **Determinism audit:** the inject is in `operator_audit` (the command, D15) and on the feed as a `PlanStateChanged` `replanning`→new-plan transition. **Default mechanism (as built):** when `InjectPlanner` is nil the executor uses a deterministic default — a single-step forward plan carrying the instruction, routed by the auction, with prior results in `masterContext`. (This refines the earlier "nil ⇒ Unimplemented" sketch: a working default is better for the demo path; the `InjectPlanner` seam is the optional LLM/remaining-steps-preserving upgrade.)

### A1.2 — Operators may widen tags, from the controlled vocabulary, audited

ADR-0034/0035 constrain *untrusted agents* to narrow-only, kernel-derived classification. The human operator has `ScopeSystem` authority (D13) and is **above** that plane.

- **Decision:** an operator may both **add (widen)** and **remove (narrow)** a memory document's `metadata.tags`. The agent narrow-only rule does not bind the operator.
- **Guard:** tags must come from the existing **ADR-0034 controlled `Vocabulary`** — an operator cannot coin an arbitrary tag (prevents typo-misclassification). The mutation goes through a kernel write path that re-stamps provenance with the operator as actor (controller, not back door — ID-3), never a raw DB write.
- **Audit:** every tag mutation is recorded with before/after and the operator actor (D15); a widening is therefore always explicit in the trail. This is the semantics 0047-25 implements.
