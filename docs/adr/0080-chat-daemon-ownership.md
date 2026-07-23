---
id: 0080
title: Chat Daemon Ownership — Dedicated Conversational Ingress Bypassing the Planner
status: Proposed
date: 2026-07-22
supersedes: []
superseded_by: []
depends_on:
  - 0031-universal-input-router
  - 0032-reactive-rule-engine
  - 0033-daemon-agent-architecture
  - 0037-managed-cognitive-resource-allocation
  - 0057-open-core-boundary
  - 0061-durable-reactive-execution
  - 0070-daemon-supervision
---

# ADR-0080: Chat Daemon Ownership

## Status

Accepted — MVP implemented and validated live on the τ²-bench airline benchmark (2026-07-22).
The same task that produced a *hollow* pass under the old planner path (all replies were
leaked `plan partially failed` errors, 0 tool calls) is now a **competent** solve under the
chat-daemon system: real `get_user_details` / `get_reservation_details` tool calls, correct
policy-compliant refund refusal, 0 leaked errors, reward 1.0.

**Implemented (MVP):**
- `cambrian-core/agents/chat_session_agent.py` — the Session agent (D2): per-turn ReAct loop
  (`final_answer`/`tool_call`/`yield_subgoal`) with the D5 spoken-only guardrail.
- `cambrian-premium/chat/manager.go` — the Chat Manager (D1) as a synchronous HTTP ingress
  (`/open`, `/turn`, `/close`); owns the per-conversation transcript (threaded via handoff
  metadata), dispatches via `CallAgent` (**planner bypassed**), provisions the per-turn
  managed-LLM token.
- **Config-driven startup:** `execution.chat_manager_addr` (kernel config) enables + binds the
  manager; the kernel brings it up at boot via the premium plugin lifecycle — not an env flag
  or manual spawn.
- **Seam addition (only core change):** `app.ReactiveServices.AcquireLLMToken`, wired from the
  LLMGateway, so a direct-dispatch consumer can provision the ADR-0018 managed-LLM budget the
  planner path issues at `server.go:493`. Without it the dispatched agent's
  `GenerateViaModelStream` is rejected `UNAUTHENTICATED: session_token_id is required` — a gap
  that means the pre-existing reactive `dispatch_agent` path never worked for LLM-calling
  daemons (consistent with `ConversationEngine` having been unwired).

**MVP simplifications vs the full design (follow-ups):** the Session agent runs as a shared
stateless-per-call `cognitive` instance with the manager threading the transcript, rather than
one supervised per-conversation daemon owning its own in-process state; and the ingress is HTTP
rather than the SDK-writable manager + agent-plane `SendTurn` RPC.

Original motivation retained below.

## Context

### The defect this fixes

The airline benchmark runs Cambrian as a customer-service agent across a multi-turn phone
conversation. Each user turn was delivered via `AgentClient.execute_task` → the core
`Server.Execute` → Planner → Auctioneer → DAGExecutor. The planner decomposed a
**conversational turn** ("reply to the customer") into **executable pseudo-steps** such as
*"Ask the customer to provide their name and booking reference."* No agent can execute
"ask the customer" — there is no such tool — so step 0 failed, replan regenerated the
identical step, replan-validation rejected the repeat, and the plan "failed at step 0". That
error string was then emitted **as the spoken reply**.

Measured result: of 24 airline tasks, the top-line solve rate was 33% (8/24) — but a
loop-level review (every agent reply inspected) showed **all 8 passes were hollow**: every
turn was a leaked `plan partially failed…` error, zero competent replies, ~3 real tool calls
across the entire run. The tasks scored 1.0 only because their correct outcome was "do not
modify the DB", which an agent that does nothing trivially satisfies. Real competent solve
rate: 0.

The root cause is architectural, not a tuning bug: **a conversational turn can fall into the
task planner by default.** No planner patch is correct here — turn-taking dialogue is a
different control loop from plan→bid→execute→verify. Conflating them means every "hi, can you
help?" pays the full orchestration tax and inherits its failure modes.

### What already exists (and what does not)

The conversation-as-daemon design was specified in ADR-0032/0033 and **~70% built**, but the
ingress→engine→daemon chain was never connected in production:

| Component | State |
|---|---|
| `reactive.ConversationEngine` (`StartConversation`/`SendTurn`/`EndConversation`/`GetHistory`) | Implemented + unit-tested, but `NewConversationEngine` is called **only in its test file** — never wired. It models chat as a watch; this ADR **drops it** in favor of the manager tier (D1/D3). |
| `Orchestrator.ChatStream` RPC (`cambrian.proto`) | Generated stub only. Server handler is `UnimplementedOrchestratorServer.ChatStream`. |
| Core `Server.Execute` `DecisionChat` branch → `ProcessSync` | Present, but reaches a daemon only if a watch was registered first — and only `StartConversation` registers it, which nothing calls. |
| `conversation_daemon` agent | **Does not exist.** Only a hardcoded `Target: "conversation_daemon"` string in `StartConversation` and a name in the ADRs. Not in `cambrian-core/agents/`. |
| Daemon lifecycle (spawn-per-stream, supervision, flap-quarantine) | Built — ADR-0033 / ADR-0070. |
| Durable per-turn journal | Built — ADR-0061. |
| Escalation to the planner from inside an agent loop | Built — `yield_subgoal` → YieldCoordinator (ADR-0037 D10). |

## Decision

**Chat is owned by a two-tier daemon hierarchy — a Chat Manager daemon that spawns and
forwards to per-conversation Session daemons. The core `Execute`/Planner path never receives
chat, and there is no Go-side conversation logic.** A conversational turn cannot fall into the
planner because it never enters the planner's front door.

This is the gateway + per-session-actor pattern (OTP supervisor→gen_server, Akka/Orleans
grains, Phoenix Channels). **`reactive.ConversationEngine` is dropped**, not wired: it modeled
a chat as a reactive *watch* (`StartConversation`=RegisterWatch, `SendTurn`=signal→ProcessSync,
`EndConversation`=DeleteWatch) — a different, heavier design that routes chat through the Go
reactive engine. The manager tier supersedes it with no Go dialogue/session logic.

### D1 — Chat Manager daemon (pluggable; the ingress)

A long-running **Chat Manager** is the front door: it owns the inbound network listener,
receives every chat message, applies **user-authored** logic (authentication, tenant routing,
rate-limiting), and forwards each message to the correct per-conversation Session daemon.
Users can ship their own manager; Cambrian provides a default. Contract a custom manager
implements: `receive(message, conversation_id, auth) → forward → reply`, plus idle-session
cleanup.

The manager is an unusual daemon — a *server*, not the classic ADR-0033 signal *producer* —
so this ADR makes "inbound-server daemon" a first-class daemon shape with an explicit
receive→forward→reply contract.

### D2 — Per-conversation Session daemon (new; holds all dialogue + state)

The manager summons one **Session daemon per conversation** via the existing
`AgentManager.SpawnDaemon(agentID, streamID, params)` — which already forks the process,
assigns a **UDS socket running a health-checked gRPC server**, and supervises it (ADR-0070).
Because the spawned daemon is a directly-addressable gRPC server, the manager forwards turns
**straight to the session daemon's socket** (request/response); manager→session traffic does
not detour through kernel dialogue logic.

Each Session daemon is a request/response `CognitiveAgent` running the per-turn ReAct loop and
**owns its own conversation state/history** (in-process, persisted via the SDK memory client —
no Go-side session store). Its loop chooses exactly one action per turn:

- **`final_answer`** — speak to the user (greeting, clarifying question, policy refusal, or an
  answer it already has).
- **`tool_call`** — invoke a domain tool (e.g. airline MCP tools) to look up or mutate state.
- **`yield_subgoal`** — escalate a genuine multi-step task to the kernel planner via the
  existing YieldCoordinator (ADR-0037 D10). *Running the planner is an action the session
  daemon chooses, never the default every message takes.* It narrates the result back
  conversationally.

The decision policy between these three is the real design surface (see Consequences).

### D3 — The irreducible Go surface is process lifecycle only

The only Go the design requires is `AgentManager.SpawnDaemon/StopDaemon` (process fork, UDS
socket assignment, health-gated boot, supervision, cleanup) — which already exists and every
daemon uses. A userland manager must **not** reimplement OS process supervision; it drives the
kernel spawner and receives an address. There is **no** conversation-specific Go code: no
`ConversationEngine`, no watch, no signal-modeling of chat. The manager owns idle cleanup
(ref-count / idle-timeout → `StopDaemon`).

### D4 — Chat never enters the planner's front door

Chat reaches a Session daemon only through the manager. To make the invariant enforceable, the
core `Execute` path treats a request carrying `_conversation_id` as a routing error, and the
`DecisionChat` branch in `Execute` is demoted to a compatibility shim scheduled for removal.
Net: no path decomposes a conversational turn in the planner.

### D5 — Error-leakage guardrail lives in the chat layer

Internal kernel/planner/daemon errors must never be emitted as spoken text. The Session
daemon's response assembly (chat layer, not core) wraps reply production: on any internal
failure it substitutes a safe fallback ("Sorry — could you say that once more?") and logs the
real error out-of-band. This also suppresses `Observations:` / `<thought>` / workspace-markup
leakage. Deliberately **not** a core change — the planner has no notion of "spoken to a user".

## Consequences

**Scope of work (answering "do we only need a new daemon agent?"): a manager + a session
daemon — two new agents, no Go dialogue/session code.**
- New: **Session daemon** SDK agent + manifest (D2, D5) — the bulk of the behavior.
- New: **Chat Manager** daemon (D1) — a default shipped by Cambrian, overridable by users. It
  is the inbound server + auth/routing + spawn/forward/idle-cleanup driver.
- Reused as-is: `AgentManager.SpawnDaemon/StopDaemon` (process lifecycle), daemon supervision
  (ADR-0070), durable journal (ADR-0061), `yield_subgoal`→YieldCoordinator (ADR-0037 D10).
- Dropped: `reactive.ConversationEngine` (never wired; superseded by the manager tier).
- Core: no behavior change; D4 is a guard (reject `_conversation_id` on `Execute`) that can
  land later.

**The per-turn decision policy is the risk.** Too eager to `yield_subgoal` and we are back in
the planner loop; too reluctant and the daemon never calls tools (the *other* airline failure
— it barely called any). This policy must be explicit (prompt + few-shot + a bias toward
`tool_call` for lookups/mutations and `yield_subgoal` only for genuinely multi-step goals),
and it is the thing the benchmark should measure.

**Two open trade-offs to settle in the design slice:** (a) manager→session transport — direct
to the session daemon's UDS socket (leanest, preferred) vs. kernel-mediated dispatch; (b)
process-per-conversation scaling — fine at benchmark/normal load, heavy at thousands of
concurrent chats, where pooling or in-process sessions would be needed. ADR-0033 already
accepts process-per-stream, so (b) is consistent, not new.

**Benchmark validation.** The airline driver switches from `execute_task` to the Chat Manager
ingress (open conversation per task, send each turn). Success = competent (non-hollow) solve
rate replaces the current 0. This is the acceptance test for the ADR.

**Open-core boundary (ADR-0057) preserved.** The Chat Manager and Session daemons are
premium/SDK-land; core provides only substrate they call: the daemon spawner, tool registry,
memory/retrieval, and the planner via `yield_subgoal`. Core stays task-only.

**Does not touch:** the planner, the auctioneer, the DAG executor, or retrieval. This ADR
changes *what reaches* the planner, not the planner itself.
