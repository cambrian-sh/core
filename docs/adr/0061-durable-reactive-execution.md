---
id: 0061
title: Durable Reactive Execution (Signal Journal, Action Idempotency, Dead-Letter)
status: Accepted
date: 2026-07-15
supersedes: []
superseded_by: []
depends_on:
  - 0032-symbiotic-reactive-rule-engine
  - 0033-daemon-agent-lifecycle
  - 0057-open-core-boundary
---

# ADR-0061: Durable Reactive Execution (Signal Journal, Action Idempotency, Dead-Letter)

## Status

Accepted

## Context

The premium `ReactiveEngine` (ADR-0032) evaluates user `WatchConfig` conditions
against incoming signals and executes configured actions (`emit_event`, `ingest`,
`dispatch_agent`, `start_plan`). Today that pipeline is **entirely in-memory**:
`OnSignal` fan-outs the signal to bounded fast/slow work channels, a worker
evaluates the condition, and — if it passes — calls the action executor. Nothing
is persisted along the way.

This is Gap **G1** from `docs/research/daemon-watches-readiness/REPORT.md`, and it
is the load-bearing one: **watches cannot be trusted with consequential actions.**
Concretely:

- **Loss on restart.** A signal in a work channel when the kernel restarts is
  gone. A `start_plan` that was about to fire never fires, silently.
- **Double execution.** If the same logical signal is delivered twice (retry,
  reconnect, replay), the action runs twice — two plans started, two agents
  dispatched. There is no dedup.
- **Silent failure.** A failing action just logs an error
  (`reactive_engine.go` `process`). An operator has no way to see "what did my
  watch fail to do?" — no dead-letter, no read surface.
- **No replay.** There is no cursor and no journal, so there is nothing to resume
  from after a crash.

`start_plan` and `dispatch_agent` are side-effecting and often irreversible; a
lost or doubled one is a correctness bug an operator would rightly refuse to
automate against. Durable-execution systems (Restate/Temporal/Inngest) solve this
with the same three primitives: a **journal** (persist the intent before acting),
**idempotent effects** (replay-with-skips), and a **dead-letter** for what could
not be delivered. This ADR brings those three to the reactive lane.

The open-core boundary (ADR-0057, invariant #1) constrains the shape: the premium
engine must not import kernel internals. It already receives its capabilities
through the OSS-provided `app.ReactiveServices` bundle (a `WatchStore`, an
`Auctioneer`, an `EventBus`, …). Durable execution must fit the same seam.

## Decision

Add durable execution to the reactive lane with three persisted primitives,
storage in OSS core (bbolt), logic in the premium engine, connected only by a new
interface in the `ReactiveServices` bundle.

### 1. A durable signal journal (not the synaptic event log)

The issue text suggests "the event log is the natural journal", but the synaptic
event log is a **120s replay spool** (ADR-0047) — the wrong retention for
crash-replay. Instead a **dedicated durable journal** in bbolt:

- Bucket `reactive_journal`: monotonic `seq (uint64) → JournalRecord{Seq, StreamID,
  SignalJSON, ReceivedAt, TTLExpiresAt}`. `OnSignal` **appends before** condition
  evaluation — the signal survives a crash between receipt and action.
- Bucket `reactive_cursors`: `watchID → last-processed seq`, so replay is bounded
  and does not re-scan the whole journal.

### 2. Action idempotency — the exactly-once primitive

Every action execution is keyed:

```
idempotencyKey = sha256( watch_id | signal_fingerprint | window )
signal_fingerprint = sha256( StreamID | canonical(Payload) | RawText )
window            = Timestamp truncated to reactive.dedup_window
```

Bucket `reactive_idempotency`: `key → executedAt`. `MarkExecutedOnce(key)` is an
**atomic check-and-set inside a single bbolt transaction** — it returns `true`
only the first time a key is seen. The engine executes the action **only when
`MarkExecutedOnce` returns true**. This is the correctness primitive: replay,
retry, or double-delivery of the same logical signal executes the action exactly
once, and it survives restart because the marker is persisted.

The `window` bounds dedup in time: a legitimately-recurring signal in a *later*
window is a different key (so it fires again), while a redelivery of the *same*
signal within the window is deduped. `reactive.dedup_window` is configurable
(too long drops legitimate repeats; too short risks a double-execute across a slow
restart).

The **cursor is an optimization, not the correctness mechanism** — even a full
journal replay is safe because idempotency dedups anything already executed.

### 3. Dead-letter + operator read surface

Bucket `reactive_deadletter`: `id → DeadLetterRecord{ID, WatchID, ActionType, Key,
Reason, SignalJSON, FailedAt}`. Actions that fail, and journal signals that expire
past their TTL before they could run, are recorded here rather than dropped. A new
kernel-owned `OperatorConsole.ListWatchDeadLetters` read RPC (the same shape as
`QueryAudit`) exposes it so the UI/CLI can answer "what did my watch fail to do?".
Contract version bumps `0050 → 0051` (+ capability `watch-deadletter`).

### 4. Replay on start

When the engine starts, for each active watch it replays journal records after
the watch's cursor whose stream matches and whose TTL has not expired, re-running
`evaluate` (idempotency skips already-executed actions); expired records go to the
dead-letter; the cursor advances. Journal records that are both acked and past TTL
are pruned by a bounded periodic compaction so bbolt does not grow unbounded.

### Seam (open-core)

A new `ReactiveJournal` interface is added to `app.ReactiveServices`
(`AppendSignal`, `ReplayFrom`, `Get/SetCursor`, `MarkExecutedOnce`,
`RecordDeadLetter`, `ListDeadLetters`), implemented by the OSS bbolt adapter and
injected into the premium engine. The engine's `Journal` field is optional — a nil
journal preserves today's pure in-memory behavior, so OSS builds and existing
tests are unaffected.

## Consequences

**Positive.**
- Watches become trustworthy for consequential actions: exactly-once execution
  that survives restart, and a durable record of failures.
- The correctness argument is small and auditable: one atomic bbolt check-and-set.
- The premium engine stays free of kernel internals; storage stays in OSS; the
  only new coupling is one interface on the existing bundle.
- Nil-journal default means zero behavior change where durability isn't wired.

**Negative / costs.**
- Every signal now pays a bbolt write before evaluation (append) and a bbolt
  check-and-set before action. For the reactive lane's signal rates this is
  cheap, but it is not free; the fast path gains a synchronous disk write.
- The `reactive.dedup_window` is a correctness-sensitive knob (see above) —
  operators must understand it.
- Journal growth requires GC; a compaction bug could either leak storage or prune
  un-acked work. The prune only removes records that are acked **and** past TTL.
- Contract bump + ui/cli re-vendor debt (recorded as skew per ADR-0047 practice;
  the UI/CLI do not read the new RPC yet).

**Neutral.**
- Exactly-once is *effect* idempotency, not distributed consensus — appropriate
  for a single-kernel bbolt-backed engine, not a multi-node claim.
- The synaptic event log is deliberately *not* reused as the journal; if a future
  durable event store replaces bbolt, the `ReactiveJournal` port is the swap seam.

## References

- `docs/research/daemon-watches-readiness/REPORT.md` — Gap G1.
- `docs/backlog/REACT-01-signal-journal-idempotency.md`.
- ADR-0032 (reactive rule engine), ADR-0033 (daemon lifecycle), ADR-0047
  (operator transport plane / spool retention), ADR-0057 (open-core boundary).
- Durable-execution prior art: Restate, Temporal, Inngest (journal +
  replay-with-skips + dead-letter).

---

## Amendment A1 — the GC this ADR specified, actually scheduled (2026-08-01, GOV-02)

This ADR wrote, under Consequences: *"Journal growth requires GC... The prune only
removes records that are acked **and** past TTL."*

Both halves were true of `Prune`. Neither was true of the system, because **nothing
ever called it.** `Prune` shipped implemented, unit-tested, and wired to no caller;
the journal grew until someone pruned by hand. This is the fifth instance of that
exact shape in this codebase — after `SetCallerScope`, `MemoryWrittenEvent`, the
unknown-principal check and the `RememberService` ingest route — and it is worth
naming as a class: **a subsystem whose tests pass because a test supplies the
caller the product does not.** The unit test is not evidence the feature exists.

### Decisions

**A1.1 — `Prune` is bounded.** Signature becomes
`Prune(minAcked uint64, limit int) (removed int, more bool, err error)`. The
unbounded form deleted every match inside a single bbolt `Update`; bbolt serializes
writers, so a first prune over a journal grown for months holds the write lock for
the whole scan-and-delete and stalls every append behind it. The cap makes a large
backlog drain over several passes instead of one long stall, and `more` lets a
caller distinguish *capped* from *done* — seen on every tick, it says the backlog
is not draining.

**A1.2 — The floor is the lowest cursor across every REGISTERED watch**, not across
the watches that have acked. A watch registered but never advanced sits at cursor 0
and correctly holds the floor at 0; taking the minimum over only watches with a
recorded cursor would compute a floor above a watch that has consumed nothing and
prune signals it had never seen. With no watches at all, TTL alone governs — a
deployment that removes its last watch must not thereby keep the journal forever.

**A1.3 — No citation floor, and the reason is checked rather than assumed.** GOV-02
asked that a signal "cited by a dead-letter entry" survive pruning, on the model of
REC-03's evidence floor. It does not need one: `domain.ReactiveDeadLetter` embeds
the whole `Signal` **by value**, so a dead letter is self-contained and pruning
cannot orphan it. A test pins that property, so if the type ever becomes a seq
reference the assumption fails loudly instead of silently becoming wrong.

**A1.4 — A backtest states the window it searched.** `WatchBacktester.Backtest`
returns `domain.WatchBacktestResult` carrying the retained oldest/newest seq and
count alongside the verdicts (contract `0084`→`0085`). GC shortens replayable
history, which made *"would have fired twice in six months"* and *"would have fired
twice in the four days still retained"* the same reply. The window is part of the
result rather than a separate query because one a caller must remember to ask for
is one most callers will not ask for.

**A1.5 — Passes are announced on the operator feed** using REC-03's
`RetentionRunEvent` with `source="reactive_journal"`. That event was made
source-agnostic for this caller, so the second consumer of retention costs no
contract bump and an operator gets ONE answer shape to "what did this system
delete", whether the answer is record versions or journal entries.

### Status

Implemented. `reactive.JournalGC` (bounded pass, timer loop, feed announcement)
starts from the reactive plugin's `Lifecycle.Start` in its own goroutine, so `Start`
still returns promptly. Config `GCInterval`/`GCBatch`, defaults 1h / 5000, `0`
disables — a real deployment choice, but not the default, since "implemented and
never scheduled" is the defect this amendment closes.
