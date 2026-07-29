---
id: 0098
title: The Conversation Progress Channel — Telling a User What Is Happening Without Telling the Model
status: Proposed
date: 2026-07-28
supersedes: []
superseded_by: []
amends:
  - 0090-ingress-and-surface-identity
depends_on:
  - 0016-global-workspace-stage
  - 0032-reactive-rule-engine
  - 0047-operator-transport-plane
  - 0062-reactive-backpressure-storm-control
  - 0080-chat-daemon-ownership
  - 0084-conversation-model-session-pool
  - 0090-ingress-and-surface-identity
---

# ADR-0098: The Conversation Progress Channel

## Status

Proposed.

## Context

A task that arrives through an ingress can take minutes. One observed Telegram turn ran **26 ReAct rounds over roughly three minutes** before answering, and during all of it the user saw nothing — no acknowledgement, no sign of life, no way to tell a working system from a hung one. The reply, when it came, was correct. The experience was indistinguishable from failure.

The kernel already knows what is happening. `PlanStateOp` carries `active_step`, `active_agent`, and `status` on the operator feed, and ADR-0090 gave every ingress-borne conversation a bound delivery address. The machinery to say *"searching memory… running step 2 of 4…"* exists on both ends. Nothing connects them.

### The trap this ADR exists to avoid

The obvious implementation — emit progress as ordinary conversation messages — is wrong, and wrong in a way that would not show up for weeks.

Conversation messages are assembled into the next turn's context. Progress lines are high-volume, low-information, and self-referential. Twenty-six of them per turn would enter the transcript, be fed back to the model on the following turn, and crowd out the actual dialogue. The system would get measurably worse at conversation *because* it got better at reporting on itself, and the causal link would be invisible.

Anthropic's own context-engineering guidance is explicit that verbose intermediate content must not accumulate in the window, and that selective retention — collapsing tool calls, discarding conversational filler — is what keeps long sessions coherent. Progress lines are exactly the filler that guidance describes.

### Prior art — the same separation, four times over

Every mature system that solved this reached the same shape: **progress is a channel beside the record, never an entry in it.**

| System | The separation |
|---|---|
| **Slack** | `assistant.threads.setStatus` is a *status surface*, not a message. It is cleared automatically when the app posts its reply, and never becomes part of the thread. "Thinking Steps" surface reasoning live as distinct Block Kit elements rather than as chat turns. |
| **LangGraph** | Distinct stream modes, with `custom` (via `get_stream_writer()`) documented for progress signals that **"don't belong in the graph state."** State is the durable record; progress rides beside it. |
| **Temporal** | Queries report live status to the outside world and are deliberately **not recorded in the event history**, so observing a workflow cannot change what replay sees. Heartbeats carry progress for liveness and checkpointing, separately from the result. |
| **Telegram** | The idiomatic pattern is to post one placeholder and **edit it in place** with `editMessageText`, so a long task occupies one message rather than a wall of them. |

Four independent designs, one conclusion. That convergence is the strongest evidence available that the separation is load-bearing rather than stylistic.

### The transport constrains the design

Telegram's edit ceiling is roughly **five edits per message per minute** — undocumented, empirically established, and enforced with `429` plus a `retry_after` the caller is expected to honour. Every method counts toward the bot's limits, not just `sendMessage`. A naive one-message-per-step implementation would be rate-limited into failure by the transport before it ever annoyed a user.

So the emission rate is not a matter of taste. **Roughly one update per twelve seconds is the physical budget**, and the design has to fit inside it.

## Decision

### D1 — Progress is a first-class kind, and it is not transcript

Introduce `MessageKindProgress` alongside the existing conversation message kinds. A progress message:

- **is delivered** through the ADR-0090 envelope, to the address the conversation is already bound to;
- **is journaled** for audit and replay, like everything else that leaves the system;
- **is excluded from context assembly** — it never enters the prompt for a subsequent turn.

The exclusion is the whole point of the kind existing. It is enforced at the context-assembly seam, not by convention at each call site, so a new caller cannot reintroduce the leak by forgetting.

**Invariant: no `MessageKindProgress` may appear in assembled model context, ever.** A test asserts this directly.

### D2 — One live update per turn, superseded not appended

A turn has at most **one** outstanding progress message. Each new update **supersedes** the previous one rather than appending beside it.

Where the transport can edit in place (Telegram `editMessageText`, Slack `chat.update`), supersession is an edit and the user sees a single line evolving. Where it cannot, the ingress may coalesce or drop — the contract is that the kernel emits *supersedable state*, not an append-only log, and the ingress decides how to render it.

This is what keeps a 26-round task to one message instead of twenty-six.

### D3 — The final reply clears the progress

When the turn's real reply is delivered, the progress message is cleared or replaced. A user must never be left with "running step 3 of 4…" as the last thing on screen after the answer has arrived.

Slack does this automatically on reply and it is the right default: progress is scaffolding, and scaffolding comes down.

### D4 — Emission is debounced against the transport's ceiling

Progress emission is rate-limited per conversation, with a **default interval of 12 seconds** (one update per ~12s ≈ Telegram's five-per-minute ceiling with headroom). Updates arriving inside the window replace the pending one rather than queueing — the user wants the *latest* state, never a backlog of stale ones.

ADR-0062 already built debounce and rate limiting for the reactive lane; this reuses that machinery rather than growing a second implementation.

An ingress declares its own ceiling if it differs. A `429` with `retry_after` from any transport widens the interval for that conversation rather than retrying into the limit.

### D5 — Progress is best-effort and structurally cannot fail the task

A progress delivery that fails is logged and dropped. It is never retried into the dead-letter path, never surfaced as a task error, and never blocks the step it describes.

Stated plainly because the opposite is a tempting mistake: **the observer must not be able to break the observed.** A telemetry channel that can fail a customer's task is worse than no telemetry channel.

### D6 — Opt-in per ingress, not global

Progress is enabled per registered ingress. The operator console surfaces it as a property of the ingress registration.

Reasons it must be opt-in: a benchmark driver would have its transcripts polluted by progress it never asked for; a customer-facing surface may want silence rather than a view of internal steps; and progress leaks *shape* information about how the system works, which is a disclosure decision an operator should make deliberately rather than inherit.

Default **off**. A surface that says nothing is a smaller surprise than one that suddenly narrates itself.

### D7 — What an update carries, and what it must not

```go
type ProgressUpdate struct {
    ConversationID string
    Step           int    // 1-based, for "step 2 of 4"
    TotalSteps     int    // 0 when the plan is not yet known
    Phase          string // a short, human-facing verb phrase
    UpdatedAt      time.Time
}
```

`Phase` is a **closed vocabulary of human-facing phrases** — "understanding the request", "searching memory", "running a tool", "writing the answer" — mapped from internal state at the emission seam.

It is never raw internal state. Agent ids, capability strings, tool names, plan internals, and model names do not cross this boundary. Two reasons, and the second is the serious one: internal names are meaningless to an end user, and on a customer-facing surface they are an information leak about the deployment's structure. The mapping is where that boundary is enforced.

### D8 — The bridge lives in premium; the seam lives in core

The OSS kernel gains a narrow port — a progress sink the plan executor calls at step transitions — with a **no-op default**. The kernel emits; it does not decide whether anyone is listening.

The bridge that subscribes to plan-state transitions, applies the phase mapping, debounces, and calls ADR-0090 delivery is a **premium plugin**. Consistent with ADR-0057: the conversational surface is commercial, the seam is not.

The same reasoning as ADR-0085's PEP/PDP split — the kernel always emits, the plugin decides what becomes of it.

## Consequences

**Positive.**
- A minutes-long task stops being indistinguishable from a hung one, which is the single largest UX defect in the ingress path today.
- The transcript and the model's context stay clean by construction rather than by discipline.
- Debounce is inherited from ADR-0062 instead of reimplemented.
- The phase vocabulary gives one place to enforce what a customer-facing surface may learn about internals.

**Negative / costs.**
- A new message kind touches the conversation model, the delivery path, and context assembly — three seams, all load-bearing.
- Supersession semantics are transport-dependent; an ingress that cannot edit gets a worse experience, and that asymmetry has to be documented rather than hidden.
- The phase mapping is a maintenance surface: a new plan phase with no mapping must degrade to a generic phrase, never to a raw internal string.
- Progress is journaled, so a chatty conversation costs journal volume for data that is deliberately never replayed into context.

**Neutral.**
- Default-off means existing deployments see no change until an operator opts in.
- Telegram's edit ceiling shapes the default interval, but the interval is per-ingress, so a transport with different limits is configuration rather than a redesign.

## Alternatives considered

**Progress as ordinary conversation messages.** Simplest, and rejected: it poisons the context window (see Context), and it produces one message per step on transports that cannot edit. This is the alternative most likely to be reached for by accident, which is why it is named here.

**Typing indicators only** (Telegram `sendChatAction`, ~5s of "typing…"). Cheap and honest about liveness, but says nothing about *what* is happening and expires long before a three-minute task completes. Worth keeping as a complement for short waits; insufficient alone.

**Streaming tokens instead of phases.** Solves a different problem — perceived latency of one generation, not visibility across a multi-step plan — and would be far worse against Telegram's edit ceiling. Orthogonal; revisit separately.

**Operator-feed only.** The information already exists on the operator plane. But the person waiting is on Telegram, not in the console, and telling the operator instead of the user answers the wrong question.

## Migration

| Phase | Scope |
|---|---|
| **1. Kernel seam** | `MessageKindProgress`, the progress sink port with a no-op default, and the context-assembly exclusion plus its invariant test. No behaviour change. |
| **2. Premium bridge** | Plan-state subscription, phase mapping, per-conversation debounce, delivery through ADR-0090. Default off. |
| **3. Ingress rendering** | Supersession via `editMessageText` in the Telegram ingress; `429`/`retry_after` widening; the final-reply clear (D3). |
| **4. Operator surface** | The per-ingress toggle (D6) and the phase vocabulary rendered in the console. |

Phase 1 is independently shippable and inert. Phase 3 is where the user-visible win lands.

## References

- ADR-0090 (ingress and surface identity — the delivery envelope this rides on), ADR-0084 (conversation model), ADR-0080 (chat daemon ownership), ADR-0062 (debounce and rate limiting), ADR-0057 (open-core boundary), ADR-0047 (operator transport plane).
- Slack, [`assistant.threads.setStatus`](https://docs.slack.dev/reference/methods/assistant.threads.setStatus/) and [thinking steps for AI agents](https://slack.dev/slack-thinking-steps-ai-agents/) — status as a surface separate from the thread, cleared on reply.
- LangGraph streaming modes — [`custom` for progress that does not belong in graph state](https://d2apczqz24upf4.cloudfront.net/courses/langgraph-prod/module-11/).
- Temporal — [Queries are not recorded in event history](https://temporal.io/blog/very-long-running-workflows); [heartbeats carry progress and enable checkpointing](https://docs.temporal.io/encyclopedia/detecting-activity-failures).
- Telegram — [Bot API `editMessageText`](https://core.telegram.org/bots/api); [rate limits and the ~5 edits/message/minute ceiling](https://botnamefinder.com/blog/telegram-bot-rate-limits-explained); [`429` / `retry_after` handling](https://grammy.dev/advanced/flood).
- Anthropic, [effective context engineering for AI agents](https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents) — why verbose intermediate content must not accumulate in the window.
