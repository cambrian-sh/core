---
id: 0071
title: Watch Observability — Per-Watch Metrics, Dry-Run, Journal Backtesting
status: Accepted
date: 2026-07-16
supersedes: []
superseded_by: []
depends_on:
  - 0061-durable-reactive-execution
  - 0032-symbiotic-reactive-rule-engine
  - 0047-operator-transport-plane
---

# ADR-0071: Watch Observability

## Status

Accepted (per-watch metrics + dry-run + backtest delivered; a Prometheus/OTel export of
the per-watch series is a follow-up)

## Context

Gap **G5**: watches are write-and-pray. There are no per-watch fire/suppression counters,
no cost attribution, no way to test a watch before arming it, and no way to ask "what
would this watch have done last week." REACT-01 gave the reactive lane a durable signal
journal — the raw material for backtesting — but nothing consumed it for observability.

## Decision

Three capabilities in the premium `ReactiveEngine`, surfaced through the OperatorConsole
plane (the established read-RPC pattern), so a CLI/UI can show them.

### 1. Per-watch metrics

The engine keeps per-watch atomic counters (`reactive/metrics.go`): signals seen,
condition fired vs suppressed, dry-run would-fires, action failures, dead-letters, and
condition-evaluation count + total latency (mean = total/count). They are incremented in
the single `process` path, so every lane (durable, in-memory, dry-run, shed) is counted
consistently. `MetricsSnapshot()` / `WatchMetrics()` expose them.

### 2. Dry-run mode (`WatchConfig.DryRun`)

A watch may be registered with `dry_run = true` (a field on `WatchConfigOp`, settable via
`RegisterWatch`). The engine evaluates the condition and records a **would-fire**, but
**never executes the action** — so an operator can arm a watch in observation mode
("would have fired 3× today") before letting it act. Handled in `process` after the
condition passes and before any budget claim or idempotency mark.

### 3. Backtesting

`ReactiveEngine.Backtest(cfg, afterSeq)` replays the journaled signals (REACT-01
`ReplayFrom`) whose stream matches a candidate `WatchConfig`, runs each through the
**same** condition evaluation the live path uses (including the REACT-03 payload-key
allowlist), and returns a per-signal verdict (`would_fire` / `eval_error`) — **without
executing any action**. It answers "what would this watch have done over the journaled
history."

### Operator surface

Two read RPCs on `OperatorConsole` — `GetWatchMetrics` and `BacktestWatch` — plus the
`dry_run` field on `WatchConfigOp`. Backed by two ports (`domain.WatchMetricsReader`,
`domain.WatchBacktester`) that the premium engine satisfies and app.go wires via a type
assertion on the injected signal receiver (nil in OSS ⇒ Unimplemented — the same
excisability pattern as `ListWatchDeadLetters`). Contract bumps `0053 → 0054` (+
capability `watch-observability`).

## Consequences

**Positive.**
- Watches are no longer write-and-pray: an operator can see per-watch activity and cost,
  arm a watch in dry-run first, and backtest a candidate against real journaled history —
  the three things G5 called out.
- Backtest reuses the live condition path, so a backtest reflects real behavior (not a
  reimplementation that could drift).
- Zero live-routing change: metrics are passive counters, dry-run is opt-in per watch,
  backtest never acts.

**Negative / costs.**
- The metrics are surfaced over the operator plane (`GetWatchMetrics`), **not** yet as a
  Prometheus/OTel scrape — a native per-watch OTel series would need the per-watch
  counters threaded through the telemetry bridge across the open-core seam (the
  `TelemetryObserver` is a fixed core interface). That export is the documented follow-up
  for acceptance item 1's literal "Prometheus scrape."
- Per-watch counters are in-memory (reset on restart); durable metric history would ride a
  future metrics store.
- Backtest cost scales with the journal window and, for `llm` conditions, spends real LLM
  calls — callers scope `after_seq`.

**Neutral.**
- Contract bump + ui/cli re-vendor debt (recorded as skew, per prior practice).

## References

- REACT-05 (`docs/backlog/REACT-05-watch-observability.md`);
  `daemon-watches-readiness/REPORT.md` G5; `internal/telemetry/bridge.go`. ADR-0061
  (journal — backtest source), ADR-0032 (reactive engine), ADR-0047 (operator plane).
