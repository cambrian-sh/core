# Chat Manager ↔ Session Contract (draft)

Companion to **ADR-0080** (Chat Daemon Ownership). Defines the two interfaces of the
two-tier chat hierarchy so a Chat Manager (default or user-authored) and a Session daemon
can be built independently.

```
            external chat traffic
                    │
                    ▼
        ┌───────────────────────┐
        │   Chat Manager        │  auth · routing · idle cleanup   (pluggable)
        │   (a server daemon)   │
        └───────────────────────┘
           │ Leg A: lifecycle           │ Leg B: per-turn
           │ (OpenSession/CloseSession) │ (SendTurn)
           ▼                            ▼
        ┌───────────────────────────────────────┐
        │   Kernel (AgentManager + dispatch)     │  process lifecycle · scope · grants
        └───────────────────────────────────────┘
                    │ scoped CallAgent (Execute)
                    ▼
        ┌───────────────────────┐
        │  Session daemon        │  one per conversation · owns dialogue + state
        │  (CognitiveAgent)      │
        └───────────────────────┘
```

The manager never talks to a Session daemon's socket directly — it goes **through the
kernel**, so the substrate's scope/grant/telemetry enforcement (SEC-01, ADR-0034/0035) is
preserved. This is the one deliberate departure from "manager forwards straight to the
daemon": in this security-conscious architecture, agent→agent traffic is always kernel-
mediated. Conversation *logic* still lives entirely in the Session daemon; the kernel legs
are pure lifecycle + routing (no dialogue code).

---

## Leg A — Manager ↔ Kernel (session lifecycle)

Backed by the existing `AgentManager.SpawnDaemon/StopDaemon` (ref-counted by `streamID`).
`conversation_id` **is** the `streamID`, so a second `OpenSession` for a live conversation
attaches to the running daemon rather than spawning a second process.

| Op | Request | Response | Backed by |
|----|---------|----------|-----------|
| `OpenSession` | `conversation_id`, `session_agent_id` (default `chat_session`), `params` (policy, domain, …), `scope` | `{ok}` | `SpawnDaemon(session_agent_id, conversation_id, params)` |
| `CloseSession` | `conversation_id` | `{ok}` | `StopDaemon(conversation_id)` (decrement ref) |

`params` seed the daemon at spawn (e.g. the airline policy text, domain id). `scope` carries
the ADR-0034 tag sets; the kernel enforces `agent_scope`, advisory caller tags travel via the
reserved `_scope` payload key.

## Leg B — Manager ↔ Kernel (per turn)

| Op | Request | Response |
|----|---------|----------|
| `SendTurn` | `conversation_id`, `message`, `metadata{turn_index, _scope, …}` | `{reply, confidence, ended}` |

The kernel routes `SendTurn` to the Session daemon bound to `conversation_id` via the
standard scoped `CallAgent`/`Execute` path (sticky by `conversation_id`). Auto-opens on first
turn if no session exists (spawn-on-first-use), so a minimal manager may skip explicit
`OpenSession`. `ended=true` lets the daemon signal the conversation is complete (e.g. after a
transfer-to-human) so the manager can `CloseSession`.

### Fulfilment options (same surface, two homes)

- **In-process (default manager, premium Go):** the manager calls `SpawnDaemon` + scoped
  dispatch directly — **no proto change**. Fastest path; used first for the airline benchmark.
- **Over gRPC (user-authored managers):** the three ops are exposed as agent-plane RPCs
  (`OpenSession`/`SendTurn`/`CloseSession`) so an SDK manager can drive them. This is a
  `cambrian.proto` addition + operator-contract bump — a follow-up, not required for the MVP.

Either way the *contract* is identical, so the Session daemon and the airline driver do not
change when the manager's home changes.

---

## Session daemon contract (`chat_session`)

A registered `CognitiveAgent` (trait `cognitive`), **one instance per `conversation_id`**,
spawned/supervised by the kernel (ADR-0033/0070), journaled (ADR-0061).

**Input (per turn):** an `Execute` `Handoff` whose payload is the user message and whose
metadata carries `conversation_id`, `turn_index`, and the reserved `_scope` keys. Spawn-time
`params` (policy, domain) are available as `self.params`.

**State:** the daemon owns its conversation history — held in-process (it *is* the single
process for this conversation) and persisted to LTM via the SDK memory client keyed by
`conversation_id` (durability + cross-restart recall). **No Go-side session store.**

**Per-turn loop:** the SDK ReAct loop (`run_think`), which already offers exactly the three
actions the daemon needs — the loop chooses one per turn:
- `final_answer` — the spoken reply.
- `tool_call` — a domain tool (e.g. airline MCP tools) to look up / mutate state.
- `yield_subgoal` — escalate a genuine multi-step task to the kernel planner via the
  YieldCoordinator (ADR-0037 D10); the daemon narrates the result back conversationally.

**Output (per turn):** an `AgentResult` whose data is **only** the words spoken to the user.
The response-assembly guardrail (ADR-0080 D5) guarantees no internal error / `Observations:` /
`<thought>` / workspace markup ever reaches the user; on any internal failure it substitutes a
safe clarifying fallback and logs the real error out-of-band.

**Decision policy (the real risk surface):** bias toward `tool_call` for any lookup/mutation
of authoritative state (never invent reservation/user facts); reserve `yield_subgoal` for
genuinely multi-step goals; otherwise `final_answer`. This policy is what the airline
benchmark measures — too-eager yield reproduces the planner loop, too-reluctant never calls
tools.

---

## Airline benchmark wiring (acceptance test)

The airline driver switches from `AgentClient.execute_task` (which went through the planner
and produced 0 competent solves) to the Manager ingress:
1. `OpenSession(conversation_id=task_id, params={policy, domain:"airline"})` (or rely on
   spawn-on-first-use).
2. Per user turn: `SendTurn(task_id, message)` → spoken reply → hand to the τ²-bench hub.
3. `CloseSession(task_id)` at end; evaluate reward.

Success = the competent (non-hollow) solve rate replaces the current 0.
```
```
