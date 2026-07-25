---
id: 0084
title: Conversations — First-Class Model, OSS Session Pool, and the Pluggable Manager
status: Proposed
date: 2026-07-24
supersedes: []
superseded_by: []
depends_on:
  - 0080-chat-daemon-ownership
  - 0082-additive-licensed-plugins
  - 0047-operator-transport-plane
  - 0034-tag-based-isolation
  - 0033-daemon-agent-architecture
  - 0018-managed-cognitive-resource-allocation
  - 0062-reactive-backpressure-storm-control
  - 0070-daemon-supervision
---

# ADR-0084: Conversations, the Session Pool, and the Pluggable Manager

## Status

Proposed — **Phase 1 implemented** (see Migration). **Amends ADR-0080**, which remains the
architecture-of-record for *why* chat bypasses the planner. That decision is not weakened
here — it is the load-bearing result of this whole line of work and this ADR preserves it
exactly.

**Phase 1 landed (D1):** `domain.Conversation` / `Message` / `ConversationProfile` and the
`ConversationStore` port; `PgConversationStore`; migration `0002_conversations.sql`;
`Kernel.Conversations`, which is nil-with-a-warning on an unmigrated database so chat
surfaces disable without taking the kernel down. `Seq` assignment is race-free by
conversation-row lock (`UPDATE … next_seq+1 … RETURNING` inside the append transaction) and
`ClientID` provides retry idempotency without consuming a sequence number. Verified against a
live Postgres — 6 integration tests including a 20-goroutine concurrency test that a
`SELECT MAX(seq)+1` implementation would fail — plus 6 pure-domain tests. The integration
test applies the **real migration file** rather than an inline copy, so it cannot drift from
shipped DDL.

**Phase 2 landed (D4/D5-OSS-half):** `internal/agentpool` — a bounded pool of
interchangeable workers with an admission gate that sheds at `Size+QueueSize` rather than
queueing without bound, load-balanced (non-sticky) dispatch, all-or-nothing start, and a
deliberate split between a **lost** worker (not live ⇒ respawn, `ErrWorkerLost`, the turn
never ran) and an **execution** failure (worker still live ⇒ surfaced as-is, no respawn).
`internal/chat.TurnService` composes it with the Phase-1 store into the OSS turn path, emits
the ADR-0080 handoff contract unchanged, and is retry-safe via `ClientID`. Wired behind
`execution.chat_pool_size` (default 0 = disabled) as a kernel-contributed `Lifecycle`.
16 tests, race-clean.

One deliberate refinement to D4: **the pool does not itself re-dispatch a lost turn.** D4
said a lost worker's turn is re-sent, but a turn may already have executed side-effecting
tool calls before the failure, and only the caller knows whether re-running them is safe.
The pool therefore self-heals and reports `ErrWorkerLost` precisely; re-send is the manager's
policy call in Phase 3. Mechanism in the kernel, policy in the manager.

**Phase 3 landed (D9, operator-plane half):** the OSS chat lane on the operator plane.
`operator.proto` gains `OpenConversation` / `SendTurn` / `CloseConversation` /
`ListConversationMessages` (contract **0060 → 0061**, capability `chat`, advertised only when
the pool is on). `SendTurn` routes through `internal/chat.TurnService` — the worker pool — and
**not** `k.Server.Execute` (the planner). `conversation_id` is client-supplied so Open is
idempotent on it; ownership is stamped from the resolved operator principal and enforced on
every call (a non-owner gets PermissionDenied, never a leak of existence). Migration `0003`
adds an optional per-conversation `policy`. Verified: 7 handler tests (idempotency,
ownership, unwired ⇒ Unimplemented), codegen idempotent, store integration tests green with
both migrations.

Two honest scoping notes:

- **The old `SendMessage`/task-session path still uses the planner — by design.** This phase
  did not delete it; it added the *conversation* surface alongside it. A task session
  (`CreateSession`/`SendMessage`) is goal-oriented work and the planner is correct there; a
  *conversation* (`OpenConversation`/`SendTurn`) is a chat turn and takes the single-loop pool
  path. That is exactly the D2 distinction, now expressed on the wire.
- **Not yet built:** the D6 agent-plane `SendTurn` + SDK-writable daemon-agent manager (the
  premium-facing half of Phase 3), and the UI/CLI surfaces. Contract skew is recorded below.

**Phase 4 (partial) landed — the session-noun overload:** `domain.SessionToken` /
`SessionState` are renamed to `BudgetLease` / `BudgetLeaseState` (they are a per-STEP LLM
spend lease, ADR-0018 — never a conversation or login session; that shared name was the worst
of the overload). `domain.Session`'s doc no longer claims to be a "conversation container" —
it is a **task** container, and `domain.Conversation` (Phase 1) is the chat entity. The method
`GetSessionState` → `GetBudgetLeaseState` for coherence.

Scope, stated honestly: the rename is **Go domain vocabulary only**. The config-schema keys
(`session_token_ttl_multiplier`, `session_token_sweep_interval_seconds`, `max_session_tokens`)
and the wire/proto names (`_session_token_id`, `session_token_id`) **keep their names** — they
are held-stable contracts (ADR-0057 D8), and renaming them would be a breaking change needing
its own deprecation cycle. So the domain code now reads unambiguously while the external
contracts are untouched. Cross-repo: premium consumes `domain.SessionToken`, so its references
were updated in the same change (ADR-0057 D13 seam coordination). **Still deferred:** the
`streamID` split (reactive-stream vs conversation identity) and any physical rename of the
`Session` type itself — both high-churn, low-marginal-value, and not blocking.

Not yet built: the `streamID` split (rest of Phase 4), Phase 5, and the D6 manager half of
Phase 3.

## Contract skew (recorded per change-control)

Core now serves contract **0061**. The UI pins **0060** (`ui/src-tauri/src/pb.rs`) and the CLI
vendors **0047**; neither has re-vendored the conversation RPCs. This is safe and deliberate:
the new RPCs are purely additive, an un-vendored client simply never calls them, and the
`chat` capability is advertised only when the pool is enabled, so a UI hides the surface as it
does for any capability it lacks. Re-vendoring + a UI chat surface is separate UI-repo work
(the D9 UI half), sequenced with the D6 manager rather than blocking this kernel change.

**The OSS chat worker ships as `agents/chat_agent.py`** (the pool's default agent id), closing
the gap where OSS had a pool but no agent to run in it. It owns one turn in a single ReAct
loop, is stateless per call, honours the conversation's recall posture (D7), reads generic
tool-posture flags (`tools_full`, `max_tool_rounds`) off the turn, and carries a spoken-only
guardrail so an internal error can never be read out to a user. It is deliberately GENERIC:
domain behaviour arrives as `policy` data, never as code in the file.

**Two agents, on purpose — the benchmark's agent is not in OSS.** The premium chat manager
continues to dispatch to the premium `chat_session_agent`, which carries the customer-service
and airline-domain specifics. Benchmark/domain-specific behaviour stays out of the OSS package
by rule; the generic `chat_agent` is what OSS ships. So the τ²-bench airline path is unchanged
by this work — it still uses the premium agent — and no airline re-run is required for the
Phase-2 pool, which the benchmark does not exercise.

What changes is everything around it: conversation state moves from manager memory into the
kernel, execution moves from one process per conversation to a bounded pool, and the manager
moves from an in-process Go ingress to an SDK-writable daemon agent.

*(Housekeeping: ADR-0080's frontmatter says `Proposed` while its body says `Accepted`. The
body is correct — it shipped and was validated live. That drift should be fixed.)*

## Context

ADR-0080 fixed a category error: Cambrian routed *every* external request through the task
planner, so a conversational turn — "reply to the customer" — was decomposed into pseudo-steps
like *"Ask the customer for their name."* No agent can execute that, so step 0 failed, replan
regenerated the identical step, and **the resulting `plan partially failed` string was spoken
to the customer.** Airline went from 0% competent solves to 30/50 (60.0%) once turns were
owned by a single agent loop with the planner reachable only via explicit `yield_subgoal`.

**That rule is inviolable here.** Nothing below may reintroduce planner decomposition of a
conversational turn, and nothing may restore chat-as-a-watch (`conversation_engine.go`,
deliberately removed by ADR-0080).

Three things nevertheless block chat from being a customer-facing product.

### Gap 1 — there is no conversation model

There is **no `Message` or `Conversation` entity anywhere in the OSS kernel.**
`domain.Session` describes itself as "a persistent conversation container" but is a *task*
container: `Goal`, `Status` (active/paused/dormant/completed), `Summary`, and it holds plan
executions. On the operator plane `CreateSession` is wired but `SendMessage` is
`Unimplemented` (there is a test asserting exactly that). The premium manager worked around
the absence with an in-memory `transcript []string` that dies on restart.

So OSS chat is not "partially built" — it is unbuilt, and the missing piece is a data model.

### Gap 2 — one OS process per conversation

ADR-0080 shipped Option B (daemon per conversation). `docs/future/chat-session-architecture.md`
already judges this the wrong default:

> "**B (what we built) is elegant but the wrong *default* here.** It pays
> process-per-conversation cost for in-process state we don't need."

The session agent is **already stateless per call** — the manager threads the full transcript
every turn — so the process-per-conversation cost buys nothing, while risking memory/FD/socket
exhaustion at scale. For a public endpoint the first failure at a thousand concurrent chats is
resource exhaustion, not unauthorized access.

### Gap 3 — the manager is an MVP ingress

It is a Go `http.ServeMux` on `/open`, `/turn`, `/close` with **no auth, no TLS, no rate
limiting** — built to be driven by a benchmark. ADR-0080's own record names the fix: a
*"pluggable daemon-agent manager… depends on exposing those seams as agent-plane RPCs."*

### The `session` noun is overloaded

Six unrelated concepts share it — `domain.Session` (task container), `SessionToken`/
`SessionState` (a **per-step LLM budget lease**), the chat `conversation`, `streamID`,
`RetrievalSession`, `EvaluationSession` — across 231 `SessionID` references.

## Decision

### D1 — `Conversation` and `Message` are first-class OSS domain entities

```go
type Conversation struct {
    ID        string
    OwnerID   string          // the principal that owns this conversation (D7/D8)
    Title     string
    Status    ConversationStatus // open | closed
    Profile   ConversationProfile // D7: recall + scope posture
    CreatedAt, UpdatedAt time.Time
}

type Message struct {
    ID             string
    ConversationID string
    Seq            int64     // monotonic per conversation; the ordering key
    Role           string    // user | agent | system
    Content        string
    CreatedAt      time.Time
}
```

Persisted **by the kernel**, in OSS, and used by both the OSS chat lane and the premium
manager. A turn MAY spawn a task `Session` when the user actually orders work — a
**1:N reference, not an identity**:

```
Conversation ──1:N──> Message
     │
     └── a turn that orders work ──1:N──> Session (task) ──> Plan ──> DAG
```

Kernel-level rather than manager-level for four reasons, any one of which would be sufficient:
OSS chat needs it (Gap 1); a **daemon-agent manager cannot own durable state** (D6) — it is a
supervised process that gets restarted (ADR-0070); manager restart must rehydrate; and one
store means one execution path across OSS and premium.

### D2 — `domain.Session` becomes a task container, and says so

It stops claiming to be "a persistent conversation container." Conversations reference the
task sessions their turns spawn. Episodic memory (ADR-0029) continues to key off task-session
completion, which is now a coherent boundary rather than an accidental one.

### D3 — Rename the budget lease

`SessionToken` / `SessionState` → `BudgetLease` / `BudgetLeaseState`. It is a **per-step**
credential (planID, stepIndex, tokenLimit); sharing a noun with the user-facing conversation
concept is the single worst offender in the overload. The Go rename is mechanical and free.
The SDK-visible wire key `_session_token_id` rides on `Handoff.Context`, so it gets a
deprecation cycle (dual-read, then drop) rather than a flag-day.

### D4 — Chat executes on a bounded pool of stateless workers, shipped in the OSS kernel

Option C from the design doc, **in OSS**:

```
conversation history (kernel store, D1)
   │  per turn: any free worker — load-balanced, NOT sticky
   ▼
kernel-managed POOL of N stateless chat-session workers
   │  each worker runs one ReAct turn over (profile + history + message)
   ▼
tools (MCP) · managed LLM · yield_subgoal → planner
```

Because every turn carries its history from the kernel store, **any worker can serve any
turn**; a worker crash loses nothing and the turn is re-dispatched. Pool size `N` bounds
concurrent in-flight turns.

**OSS ships the pool** so there is exactly one execution path: OSS gets real concurrent chat,
and premium adds the manager, auth, and multi-tenancy *on top* rather than shipping a second
engine. Option B (per-conversation daemon) is retained as an **opt-in** for the rare workload
that holds expensive, non-externalizable in-process state.

### D5 — Backpressure: the kernel protects itself, premium protects the wallet

Two layers, deliberately split by what they defend:

- **OSS (kernel):** the pool has a bounded queue and a shed policy. This is a *correctness*
  requirement — an unbounded queue in front of a bounded pool is just a slower crash.
- **Premium (manager):** global LLM-per-hour and per-tenant budgets, applied *before* a turn
  ever reaches the kernel. This is where cost control and abuse defense live.

The premium layer **shares the reactive lane's hardening, and only its hardening**: REACT-02
(ADR-0062) rate/queue/shed and REACT-03 injection guarding. Stateless algorithms move to a
neutral premium package that both plugins import; the one genuinely shared *live* object — a
single global LLM-per-hour budget that chat and watches must not each get their own copy of —
is published through the ADR-0082 registry service seam with `Requires: ["reactive"]`.

**Explicitly rejected: re-modeling chat as watches/signals.** That is what ADR-0032 did and
ADR-0080 removed. "Extends the reactive engine" means *reuses its hardening*, never *adopts
its routing model*.

### D6 — The manager becomes an SDK-writable daemon agent

Replace the MVP HTTP ingress with agent-plane RPCs so a manager is a normal daemon agent
written against a typed contract. Auth, conversation ownership, tenant routing, and rate
limiting live **in the manager**, per ADR-0080's own principle: *"Custom auth, tenant routing,
or rate-limiting is a custom manager."*

Because D4 removes sticky routing, the contract is a **load-balanced `SendTurn`**, not the
spawn/route/stop triple it would otherwise have been — sequencing D4 before D6 avoids
designing an agent-plane contract we would have to break one release later.

### D7 — Recall and scope posture are properties of the conversation, not agent defaults

The session agent ships `seed_recall = False` ("ground on tools, not shared LTM"), which is
right for customer chat and wrong for Company Brain. That must not be a global default someone
flips and forgets, so it becomes a `ConversationProfile`:

| Profile | LTM recall | caller_scope |
|---|---|---|
| **operator** | on | none (full) |
| **employee** | on | narrowing, set by the manager |
| **customer** | off — tools only | narrowing, set by the manager |

The mechanism already exists and is only HITL-gated: `domain.Session.CallerScope`,
`CallerScopeProvider`, `EnablePhase2`, with effective = `caller_scope ∩ agent_scope`,
persisted **server-side** and never read from the forgeable `Handoff.Context` (ADR-0034
D13/R2). The manager supplies `caller_scope` at conversation open as the "integrating
application."

This does **not** violate the ADR-0074 Tier-3 rule that the security kernel is never
pluggable: because effective scope is an **intersection**, the manager can only ever *narrow*.
The kernel still enforces; the plugin may only request a restriction.

### D8 — Tenant isolation is per-instance; conversation isolation is not

Each tenant gets its own Cambrian instance, so tenant↔tenant isolation is an **operational**
guarantee. Within a single instance, employee A, employee B, and end-customer C still share a
kernel and a memory store — so `Conversation.OwnerID` and `caller_scope` are **load-bearing
security**, not conveniences. Per-tenant deployment must not be used to argue them away.

A richer authorization model for memory entities (inheritance over the ADR-0060 document/
section hierarchy, integrity levels for untrusted content, owner/group indirection) is
deliberately **out of scope here** and deferred to its own ADR, gated on a concrete failure of
the present model.

### D9 — No third kernel plane

- **OSS chat** rides the **operator plane** (ADR-0047) — its user *is* the operator/owner.
  Finish `SendMessage`, add conversation RPCs, add streaming.
- **Premium customer chat** never touches the kernel directly: the manager daemon is the front
  door and is a *client* of the kernel, not a plane of it.

This is why no end-user plane is needed, and it preserves ADR-0047's rule that human operators
never authenticate as agents.

## Migration

| Phase | Scope | Why this order |
|---|---|---|
| **1** | `Conversation` + `Message` (OSS, additive) + persistence | Unblocks OSS chat *and* the pluggable manager; nothing else lands cleanly first |
| **2** | Session pool (OSS) + bounded queue/shed; Option B kept opt-in | The scaling fix; small step because the agent is already stateless |
| **3** | Agent-plane `SendTurn` + typed manager contract; premium manager becomes a daemon agent with auth/ownership/tenanting | Must follow 2, or the contract encodes sticky routing |
| **4** | Session-noun refactor: `BudgetLease` rename, `streamID` split, `domain.Session` → task container | Cheaper after 2, which deletes the per-conversation `streamID` usage |
| **5** | `caller_scope` Phase-2 live wiring + `ConversationProfile` | Needs 1 and 3 in place to have a conversation to scope and a manager to set it |

Benchmark gate: the **τ²-bench airline suite is the regression gate** for phases 2 and 3 — it
is the measurement that caught the original hollow-pass failure, and a turn-ownership
regression would show up there and nowhere else. Phase 2 additionally needs a concurrency
measurement that does not exist yet (N concurrent conversations against a fixed pool).

## Consequences

**Positive.**
- Chat becomes restart-safe and resumable; the transcript stops living in process memory.
- Bounded process count replaces process-per-conversation — the actual scaling blocker.
- One execution path: premium adds policy on top of the OSS engine rather than a second engine.
- Auth/tenanting become user-writable in a daemon agent instead of hardcoded in a Go ingress.
- The `session` noun stops meaning six things.

**Negative / costs.**
- Phase 4 touches 231 `SessionID` references; the `_session_token_id` wire key needs a
  deprecation cycle because the SDK sees it.
- A pool is more plumbing than a daemon per conversation, and loses the option of in-process
  per-conversation state (mitigated by keeping Option B opt-in).
- Conversation history in the kernel means chat turns now write to the kernel store on the hot
  path; this needs measurement.
- Episodic memory's session-completion trigger must be re-pointed at task sessions.

**Neutral.**
- ADR-0080's planner-bypass rule and the `final_answer` / `tool_call` / `yield_subgoal` turn
  contract are unchanged.
- `execution.chat_manager_addr` was already removed from OSS config (ADR-0082 Phase 2, closing
  an ADR-0057 D5 violation); under D6 the manager's binding leaves kernel config entirely.

## Documents to correct when this lands

- **ADR-0080**: frontmatter status drift (`Proposed` vs body `Accepted`); the
  `execution.chat_manager_addr` reference; add the amendment pointer to this ADR.
- **`docs/design/chat-manager-session-contract.md`**: Legs A/B become the agent-plane contract.
- **`docs/future/chat-session-architecture.md`**: superseded by D4/D5 once built; its §3.4
  (manager owns durable state) is reversed by D1 (kernel owns it).

## References

- ADR-0080 (chat daemon ownership — amended here), ADR-0082 (plugin manifest/entitlement +
  registry service seam used by D5), ADR-0047 (operator plane — D9), ADR-0034/0035 (scope
  intersection + narrow-only writes — D7), ADR-0033/0070 (daemon architecture + supervision —
  D6), ADR-0018 (managed LLM budget — D3), ADR-0062 (REACT-02 backpressure — D5), ADR-0029
  (episodic memory — D2), ADR-0060 (structure graph — referenced by the deferred authorization
  work in D8).
- `docs/future/chat-session-architecture.md` (Options A/B/C analysis), `docs/design/chat-manager-session-contract.md`,
  `cambrian-premium/chat/manager.go`, `agents/chat_session_agent.py`,
  `internal/substrate/operator/chat.go`.
