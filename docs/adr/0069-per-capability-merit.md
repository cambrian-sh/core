---
id: 0069
title: Per-Capability Merit + Bounded Provisional Exploration
status: Accepted
date: 2026-07-16
supersedes: []
superseded_by: []
depends_on:
  - 0002-gatekeeper
  - 0048-capability-contract-route03
  - 0067-capability-vocabulary
---

# ADR-0069: Per-Capability Merit + Bounded Provisional Exploration

## Status

Accepted (arm-gated, default off; online enablement gated on the benchmark, per the
offline-before-online discipline)

## Context

Routing defect **D5**, two facets:

1. **Global merit is capability-blind.** `AgentProfile.SuccessRate`/`TrustScore` are one
   number per agent. An agent that is superb at PDF parsing carries that merit into
   browser auctions it has never won — the L3 Merit score can't tell "good at *this*" from
   "good in general." ROUTE-03 gave every step a `RequiredCapabilities` tag, and the
   verifier already writes a per-step quality to the event log; the pieces to score merit
   *per capability* exist but aren't connected.

2. **Provisional exploration is unbounded.** Provisional (un-interviewed) agents bypass
   Layer-2 entirely (`gatekeeper.go`) — deliberate exploration so a new agent gets a
   chance, but with no ceiling. A persistently-mediocre provisional agent keeps bypassing
   forever.

## Decision

Both behind one arm, `execution.per_capability_merit`, **default off ⇒ byte-identical**.

### 1. Capability-scoped merit

- **Stamp** the step's (first) `RequiredCapabilities` tag onto each `TaskEvent`
  (`dag_executor.go`) — the only capability the routing path already knows at write time.
- The **ProfileAggregator** groups verified events by that tag and computes a per-tag
  `CapabilityStat{SuccessRate, TrustScore, SampleCount}` with the same EWMA as the global
  profile, stored in `AgentProfile.CapabilityStats`.
- **L3 Merit** (`computeMeritBreakdown`) takes the step's required capabilities: when the
  arm is on and the agent has history for one of them, it uses that tag-scoped
  success/trust; otherwise it falls back to the global profile (no tag history, no
  required caps, or arm off ⇒ unchanged). A single required cap keys the lookup; the
  event stamps the first cap, so v1 is single-capability-scoped (multi-cap crediting is a
  refinement).

### 2. Bounded provisional exploration

- A shared `domain.ExplorationBudget` — at most **N provisional wins per capability per
  sliding window** (`provisional_exploration_budget` / `..._window_seconds`). It is
  concurrency-safe and nil-safe (nil / non-positive bound ⇒ always allowed = arm-off
  behavior).
- The **Gatekeeper** grants the provisional L2 bypass only while `budget.Allowed(cap)`;
  once exhausted, that provisional agent must pass the semantic gate like everyone else —
  exploration is *granted, not unbounded*.
- The **Auctioneer** records a provisional agent's auction *win* (not merely a bypass)
  into the same budget, so the ceiling is on real exploration outcomes.
- On exhaustion the budget fires `OnExhausted`, which publishes an
  `ExplorationBudgetExhaustedEvent` — observable so the guard metric (provisional
  time-to-first-win) can distinguish healthy exploration from starvation.

### Offline / benchmark discipline

Per-tag merit is meaningless until per-tag data accrues (the `TaskEvent.Capability` stamp
must be live for a while) — the same data-latency as ROUTE-05. So the arm ships **off**;
turning it on requires the benchmark gate (`misroute_rate` ↓ with provisional
time-to-first-win not degrading beyond the agreed bound), with a DECISIONS.md entry and an
N-sweep.

## Consequences

**Positive.**
- Merit finally distinguishes "good at the capability this step needs" from "good in
  general" — the direct D5 fix.
- Provisional exploration is bounded per capability, so a mediocre newcomer can't bypass
  L2 indefinitely, while the window still lets exploration resume.
- Fully arm-gated and default-off: no live-routing change, no regression, until the
  benchmark licenses it.

**Negative / costs.**
- The merit hot path now reads `CapabilityStats` on every scored candidate (cheap map
  lookup, and only when the arm is on).
- v1 keys by a single capability (the first required tag); a step requiring several caps,
  or an agent whose merit should aggregate across caps, is a refinement.
- The exploration "win" is recorded by matching the winning proposal to a provisional
  candidate in the auction — correct for the single-auction path; a provisional win via a
  path that skips the auctioneer would not be counted (none exists today).

**Neutral.**
- `bid_dispersion` and `misroute_rate` (ROUTE-01) are the online health metrics; the guard
  is provisional time-to-first-win.

## References

- ROUTE-06 (`docs/backlog/ROUTE-06-per-capability-merit.md`); REPORT.md D5/R4. ADR-0002
  (Gatekeeper L2/L3), ADR-0048/ROUTE-03 (capability contract — the required-caps tag),
  ADR-0067/ROUTE-04 (canonical capability tags).
