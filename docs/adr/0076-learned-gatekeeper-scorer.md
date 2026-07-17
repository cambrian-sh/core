---
id: 0076
title: Learned Gatekeeper Scorer (ROUTE-07)
status: Accepted
date: 2026-07-17
supersedes: []
superseded_by: []
depends_on:
  - 0068-bid-calibration
  - 0069-per-capability-merit
  - 0002-gatekeeper-auction
---

# ADR-0076: Learned Gatekeeper Scorer

## Status

Accepted (offline pipeline + inspectable model + default-off online arm delivered and
unit-tested). ADOPTION online is gated on a **published offline win** over the calibrated
hand-weights baseline, which needs accumulated auction-funnel + verifier artifacts — the
data-dependent gate is deliberately not forced.

## Context

`GatekeeperScore` is `w1·SuccessRate + w2·TrustScore + w3·(1/latency) − w4·cost` with
hand-set weights. OrchRM (arXiv:2606.13598) evidence says a scorer *learned* from
orchestration artifacts beats hand weights — but only a *calibrated, per-capability*
baseline is worth beating (hence the ROUTE-04/05/06 dependencies). The learning must be
offline-first and reversible.

## Decision

A small, **inspectable** learned scorer that plugs into the exact seam the hand weights
occupy, behind a default-off arm.

### Model (`internal/metabolism/routescorer`)

A **logistic regression** — chosen over GBT/MLP for a first cut because its coefficients
*are* the per-feature weights, so a learned model diffs directly against the hand weights
it competes with, and it is pure Go (no CGO). Features are the **exact fields the
Gatekeeper's `meritBreakdown` produces AND the ROUTE-02 auction funnel already exposes**:
`[success_rate, trust_score, latency_term, cost_term, provisional]`. That alignment is the
key design choice — a training sample is a *direct read* of a funnel's L3 merit entry
joined with the verifier outcome, no feature reconstruction. Features are standardized;
training is full-batch gradient descent with L2; the model is JSON-persisted with its
feature list (a schema-drift load is rejected, never scored silently).

### Offline pipeline (`cmd/route07-scorer`), reproducible from artifacts

- `extract --in results.jsonl` joins each auction's winner L3 merit breakdown with the
  verifier outcome → training samples (JSONL).
- `train --in dataset.jsonl` fits the model on a deterministic train split and prints the
  **offline comparison**: learned **AUC** vs the hand-weight baseline's AUC on the held-out
  split (AUC = threshold-free "does this scorer rank the agent that will succeed above the
  one that won't"), plus the gate verdict `adopt_learned` (learned beats hand by more than
  a noise margin). Within-noise ⇒ keep hand weights (simpler, inspectable).

### Online arm (default off)

`Gatekeeper.RouteScorer` (a structural `Score([5]float64) float64` satisfied by
`*routescorer.Model`). In `computeMeritBreakdown`, when `execution.learned_scorer` is on and
a model is loaded (`learned_scorer_model_path`), the model's score **replaces** the
hand-weighted score AND the cold-start penalty (provisional is a model *feature*, so it must
not be double-applied); the merit terms are still returned for the ROUTE-02 funnel. Arm off
or model absent ⇒ hand weights, **byte-identical**. A missing/invalid model logs a warning
and leaves hand weights in place — never a silent zero score.

## Consequences

**Positive.**
- The learned model shares one feature space with both the online decision and the funnel
  training data — no reconstruction, and the funnel we already log IS the dataset.
- Inspectable (coefficients) + pure Go + reversible (flag) — meets the ROUTE-07 acceptance
  (offline comparison published; instant rollback to hand weights).
- Byte-identical when off; the calibrated ROUTE-05/06 baseline is the thing to beat, not a
  naive one.

**Negative / deferred.**
- The **offline win is not yet demonstrated** — it needs accumulated runs with auction
  funnels (`resource_selector=auction`, routing-trace on) + verifier outcomes. The pipeline
  + model + arm are built and unit-tested on synthetic data; the DECISIONS.md gate entry
  (real learned-vs-hand AUC, both arms' run IDs) is pending that data.
- Logistic regression is the v1; a GBT/tiny-MLP upgrade is a follow-up **only if** LR wins
  offline but leaves headroom.
- No periodic-retrain job yet (offline CLI is manual); scheduling it is future work.

## References

- ROUTE-07 (`docs/backlog/ROUTE-07-learned-gatekeeper-scorer.md`); OrchRM arXiv:2606.13598.
  ADR-0068 (bid calibration) / ADR-0069 (per-capability merit) — the baseline this must
  beat. Code: `internal/metabolism/routescorer/`, `cmd/route07-scorer/`,
  `internal/supervision/gatekeeper/gatekeeper.go` (arm), `internal/config/config.go`
  (`learned_scorer` / `learned_scorer_model_path`).
