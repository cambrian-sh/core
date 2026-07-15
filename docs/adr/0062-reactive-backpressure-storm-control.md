---
id: 0062
title: Reactive Backpressure & Storm Control (Debounce, Rate Limits, Global Budget, Shed Order)
status: Accepted
date: 2026-07-15
supersedes: []
superseded_by: []
depends_on:
  - 0032-symbiotic-reactive-rule-engine
  - 0061-durable-reactive-execution
  - 0057-open-core-boundary
---

# ADR-0062: Reactive Backpressure & Storm Control

## Status

Accepted

## Context

This is Gap **G2** from `docs/research/daemon-watches-readiness/REPORT.md`. The premium
`ReactiveEngine` evaluates a condition and runs an action for **every** signal on a
matching stream. A busy directory, a webhook burst, or a chatty daemon floods the
engine; when the watch condition is `llm`, every signal becomes an LLM call — a
cost/latency amplification attack you inflict on yourself. `WatchConfig.MaxConcurrentPlans`
caps the *action* side per watch, but nothing bounds the *evaluation* side, and nothing
bounds cost across the whole reactive plane.

REACT-01 (ADR-0061) gave the lane durability and, crucially, a **dead-letter** — the
place shed load can be recorded rather than silently dropped. This ADR adds the four
storm-control levers G2 calls for.

## Decision

Add four backpressure levers, all in the premium engine, using REACT-01's dead-letter
for anything shed and REACT-01's operator surface for visibility.

### 1. Per-watch debounce / coalescing (`WatchConfig.DebounceSeconds`)

A watch may declare `debounce_seconds > 0`. The first signal opens a fixed window of
that length; subsequent signals in the window are **coalesced** (not evaluated); when
the window closes the watch fires **once** with the latest signal, carrying the
coalesced batch in `Payload["_batch"]` (a list) and `Payload["_coalesced_count"]`. This
guarantees "at most once per T seconds" per watch — the headline bound for a signal
storm. The field is additive on `WatchConfigOp` (wire-compatible) and configurable via
`RegisterWatch`.

**Durability trade-off (deliberate):** debounce runs *before* the journal, so an
in-flight coalescing buffer is ephemeral (lost on crash). Only the coalesced fire is
journaled. This is correct for storm control — the whole point is that a burst of
high-volume low-value signals collapses to one durable action, keeping the journal
small — and it is documented as a Known Gap.

### 2. Per-stream rate limit (token bucket)

Each `StreamID` gets a token bucket (`reactive.stream_rate_per_sec`,
`reactive.stream_burst`). A signal that cannot draw a token is **shed at intake** —
dropped with telemetry and a throttled `ReactiveBudgetEvent` (`stream_rate` /
`rate_limited`). It is deliberately **not** per-signal dead-lettered: this is the
crudest, highest-volume shed lane, and one dead-letter per shed signal would itself be
a storm. The throttled operator event is the visibility mechanism; per-signal
dead-letters are reserved for the lower-volume budget lanes (§3). This bounds raw
intake rate independent of any watch's debounce.

### 3. Global reactive budget (LLM evaluations/hour, plans/hour)

Two plane-wide token buckets, refilled hourly: `reactive.global_llm_per_hour` and
`reactive.global_plans_per_hour`. An `llm`-condition evaluation draws an LLM token; a
`start_plan` action draws a plan token. Exhaustion **sheds** that unit (skip +
dead-letter `budget_exhausted` / `plan_budget_exhausted`) and emits a
`ReactiveBudgetEvent` on the operator feed (throttled to at most once/min per
resource), so budget exhaustion is **visible, not silent**.

The spec names the circadian/energy machinery as the budget's natural home. That
machinery does not exist yet (circadian is memory-lifecycle only), and the engine is
premium and cannot reach kernel internals (ADR-0057). So the budget is a **premium-side
token bucket** for v1; folding it into a future core energy economy is a documented
follow-up. The `EventBus` it emits on is already provided through the `ReactiveServices`
bundle — no new seam.

### 4. Shed order

Under budget pressure, **`llm` conditions degrade first**: only the LLM-evaluation and
`start_plan` paths draw from the global budget, so deterministic / pattern / always
conditions and non-plan actions keep flowing at full rate. Per-stream rate limiting is
condition-agnostic (it protects intake), but the global budget — the expensive
resource — is spent only on the expensive lane.

### Placement

All four live in `cambrian-premium/reactive/` (`backpressure.go` for the token bucket +
debouncer; the engine wires them in `OnSignal`/`process`). The only core-side changes
are additive: `WatchConfig.DebounceSeconds` + its record/proto mapping, and a
`ReactiveBudgetEvent` domain event + `ReactiveBudgetOp` feed op (the ScoutUsefulnessOp
pattern). Contract bumps `0051 → 0052` (+ capability `reactive-backpressure`).

## Consequences

**Positive.**
- A signal storm on one watch collapses to a bounded number of LLM calls and plans
  (debounce × global budget), the G2 acceptance bound.
- Budget exhaustion is an operator-visible event, not a silent stall.
- Shed load is dead-lettered (REACT-01), so "what did my watch drop, and why" is
  answerable.
- Zero new open-core seam; the engine stays free of kernel internals.

**Negative / costs.**
- More knobs (`debounce_seconds`, stream rate/burst, two global rates). Defaults are
  permissive so an unconfigured watch behaves as today.
- Debounce buffers are ephemeral (see the durability trade-off above).
- The global budget is a coarse plane-wide bucket, not per-owner/per-tenant fairness —
  a single noisy watch can spend the whole plane's budget. Per-owner quotas are REACT-07.
- Token-bucket time bucketing is wall-clock; a clock jump can briefly over- or
  under-admit. Acceptable for a load shedder.

**Neutral.**
- Gating the global budget on `ConditionType == "llm"` / `Action.Type == "start_plan"`
  is policy branching (backpressure), not agent-to-task routing, so it is outside the
  Zero-Hardcode Rule — the same category as the existing `isSlowPath("llm")` check.

## References

- `docs/research/daemon-watches-readiness/REPORT.md` — Gap G2.
- `docs/backlog/REACT-02-backpressure-storm-control.md`.
- ADR-0032 (reactive engine), ADR-0061 (durable execution / dead-letter), ADR-0057
  (open-core boundary). Related backlog: REACT-07 (per-owner quotas).
