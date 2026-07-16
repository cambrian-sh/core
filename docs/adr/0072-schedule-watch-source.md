---
id: 0072
title: Schedule Watch Source — Cron-Driven Synthetic Signals
status: Accepted
date: 2026-07-16
supersedes: []
superseded_by: []
depends_on:
  - 0032-symbiotic-reactive-rule-engine
  - 0061-durable-reactive-execution
  - 0047-operator-transport-plane
---

# ADR-0072: Schedule Watch Source

## Status

Accepted (cron parser + scheduler + missed-fire policy + proto/operator surface
delivered; a long-horizon 24h soak-drift check is the pre-release residual).

## Context

Gap **G6**: `WatchSource.Type` supported `daemon` / `filesystem` / `webhook` /
`signal_stream` — all *event* sources. There was no *time* source. A user who wants
"every morning, summarize X" had to write and register a timer daemon purely to emit a
tick, which is exactly the boilerplate the reactive plane exists to remove. Every
ambient-agent platform ships scheduled triggers as a first-class peer of event triggers.

## Decision

Add `WatchSource.Type = "schedule"`: a watch whose signals are produced by an internal
cron-driven scheduler rather than an external signal producer. A scheduled fire is an
ordinary `domain.Signal` delivered through `ReactiveEngine.OnSignal`, so it flows through
the **unchanged** condition/action pipeline — `always` conditions (the common case),
`llm` conditions, debounce, budgets, idempotency, dead-letter, and every action type all
work exactly as they do for event signals. The schedule source is purely a new *producer*;
nothing downstream is special-cased.

### Cron parser (`reactive/cron.go`)

A dependency-free 5-field parser (minute hour day-of-month month day-of-week) plus the
`@hourly`/`@daily`/`@midnight`/`@weekly`/`@monthly`/`@yearly` shortcuts. Each field
supports `*`, `*/N`, `A`, `A-B`, `A,B`. Day matching follows standard cron **OR-semantics**
(both DOM and DOW restricted ⇒ a day matches if *either* does). `Next(after)` and
`Prev(before)` bounded-scan by the minute over a ~2-year horizon. No third-party cron
library is pulled in — the parser is ~140 lines and fully unit-tested.

### Scheduler (`reactive/scheduler.go`)

Held by the `ReactiveEngine`. Per active schedule watch it arms a self-rescheduling
`time.AfterFunc` at the next cron instant; on fire it emits a synthetic signal
(`FromAgent:"scheduler"`, payload `_scheduled_time` / `_occurrence` / `_cron`, and
`Timestamp` = the scheduled instant) via `OnSignal`, then re-arms from the *scheduled*
instant to avoid cumulative drift. Timers are armed only between `Start` and `Stop`
(re-checked under the lock on fire), so a fire never races an un-started or draining
engine. Wired through the existing lifecycle: `RegisterConfig` (schedule type only) →
`upsert`, `DeleteConfig` → `remove`, `SetConfigActive` → arm/disarm, `Start` → arm all,
`Stop` → stop all. A malformed cron is logged and the watch is dropped, never a panic.

Setting the signal `Timestamp` to the scheduled instant makes REACT-01's idempotency key
bucket on **schedule time**: two distinct scheduled instants are distinct keys (both
fire), while a re-delivery of the same instant collapses to one execution.

### Missed-fire policy (`WatchConfig.MissedFirePolicy`)

Governs a schedule watch across a kernel restart:
- `skip` (default): resume at the next future instant; nothing is caught up.
- `fire_once`: on `Start`, emit a single catch-up for the most recent missed instant
  (`Prev(now)`), bounded by `JournalTTL` staleness. This is performed **only with a
  durable journal present** — REACT-01 idempotency then dedups the catch-up against any
  fire that already happened before the restart, making it exactly-once. Without a
  journal there is no persisted state to reconcile against, so catch-up is skipped rather
  than risk a double fire.

### Daemon-lifecycle exclusion

A schedule source has no external daemon, so `WatchHandler` excludes `Type=="schedule"`
from daemon ref-counting (no spurious `SpawnDaemon`/`StopDaemon`).

### Proto / operator surface

`WatchConfigOp` gains `source_cron` (17), `source_timezone` (18), `missed_fire_policy`
(19) — a wire-compatible field addition. `WatchConfigRecord` (bbolt) and the operator
mappers (`watches_a2.go`) carry them both directions. Contract bumps `0054 → 0055` (+
capability `watch-schedule`, advertised on the same watch-handler signal as the other
premium watch surfaces).

## Consequences

**Positive.**
- Time-based triggers are now a first-class peer of event triggers; no timer-daemon
  boilerplate. `schedule` + `start_plan` gives an unattended recurring plan directly.
- Zero pipeline change: scheduled signals reuse every existing lane (condition, debounce,
  budget, idempotency, dead-letter), so behavior is consistent and already tested.
- No new dependency (pure-Go cron) and no new persistence (missed-fire rides the REACT-01
  journal via schedule-time idempotency keys).
- Byte-identical when unused: the scheduler is nil-safe/no-op for non-schedule watches.

**Negative / costs.**
- Minute granularity (standard cron); sub-minute schedules are out of scope.
- `fire_once` catch-up is exactly-once only with the REACT-01 journal wired; in a
  journal-less OSS build it degrades to `skip`.
- Contract bump + ui/cli re-vendor debt (recorded as skew, per prior practice).
- A long-horizon soak (24h, drift < tolerance) is the pre-release residual for
  acceptance; unit + catch-up integration tests cover correctness now.

**Neutral.**
- Occurrence counter is in-process (informational; resets on restart).

## References

- REACT-06 (`docs/backlog/REACT-06-schedule-source.md`);
  `daemon-watches-readiness/REPORT.md` G6. ADR-0032 (reactive engine), ADR-0061 (durable
  journal — missed-fire dedup), ADR-0047 (operator plane). Code: `reactive/cron.go`,
  `reactive/scheduler.go`, `domain/signal.go`, `internal/substrate/operator/watches_a2.go`.
