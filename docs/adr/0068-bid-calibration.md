---
id: 0068
title: Bid Calibration from Verifier Outcomes (offline isotonic, arm-gated online)
status: Accepted
date: 2026-07-16
supersedes: []
superseded_by: []
depends_on:
  - 0002-gatekeeper
  - 0011-gatekeeper-score-cost
  - 0048-capability-contract-route03
---

# ADR-0068: Bid Calibration from Verifier Outcomes

## Status

Accepted (offline machinery + arm-gated online application; online enablement gated on
offline lift, per the offline-before-online discipline)

## Context

Routing defect **D4**: the auction picks the winner by the agent's own bid `Confidence`
(`auctioneer.go` — `prop.Confidence > bestProposal.Confidence`). That confidence is an LLM
**self-assessment**, and LLM self-confidence is systematically and *uniformly*
overconfident (arXiv:2602.06948, arXiv:2601.07264) — so it barely discriminates between
candidates: everyone says ~0.9. Meanwhile the Verifier already produces a `QualityScore`
per completed step and writes it to the event log (`TaskEventRecord.VerifierScore`)
alongside the `BidConfidence` — but that outcome never flows back to correct future bids.
The correcting signal already exists; nothing new needs collecting.

## Decision

Learn a **calibration map** `self_confidence → expected verifier quality` per agent from
the event log, and (behind an arm) use the *calibrated* confidence for winner selection.

### 1. Isotonic calibration (offline)

`internal/metabolism/calibration`: a pure, dependency-free package.

- **Isotonic regression via PAVA** (pool-adjacent-violators) fits a monotonic
  non-decreasing step function to `(bid_confidence, verifier_quality)` samples. Isotonic
  (not just Platt) because the miscalibration is not assumed to be a simple sigmoid — the
  only prior we trust is *monotonicity* (a higher self-confidence should not predict lower
  quality). It is order-preserving, so it never inverts a genuine confidence ordering,
  only compresses/shifts it toward observed quality.
- **Per-agent maps with shrinkage.** A `Model` holds one isotonic curve per agent plus a
  global curve fit over all samples. `Calibrate(agentID, conf)` blends the agent curve
  with the global one by sample count: below a threshold `n` the agent map has little
  weight, so a rarely-seen agent is calibrated by the fleet prior, not by noise. (The spec
  says agent×capability; the event log carries agent + confidence + quality but not the
  step capability, so v1 keys by **agent**. Agent×capability is a refinement once the
  capability is logged on the task event.)

### 2. Offline eval first, online only on evidence

Per the offline-before-online invariant, the online application is **inert by default**:

- The arm `execution.calibrated_bids` is **off** by default; when off, winner selection is
  byte-identical (raw confidence).
- The offline artifacts — per-agent calibration curves — are produced by
  `cmd/calibration-report` (reads the event log, fits, emits JSON), satisfying "curves
  published to artifacts" **without touching live routing**.
- Only after an **offline replay shows lift** (winners shift toward verifier-preferred
  agents) is the arm turned on, with a DECISIONS.md entry. A negative offline result
  closes the issue as "measured, not wired."

### 3. Online application (arm-gated)

When `calibrated_bids` is on and a model is loaded, the auctioneer computes
`calibrated = model.Calibrate(agentID, prop.Confidence)` and selects the winner by the
calibrated value (the raw self-report is still recorded on the bid for tracing). The model
is fit from the event log at startup and refreshed periodically.

## Consequences

**Positive.**
- The winner is chosen by *expected verified quality*, not by who claims the most —
  directly attacking D4's "everyone says 0.9" non-discrimination.
- Zero new data collection; reuses the verifier signal already in the log.
- Monotonic-by-construction: calibration can compress the confidence range (raising
  dispersion) but never invert a legitimate ordering.
- Default-off + offline-first: no live-routing change until offline lift is proven.

**Negative / costs.**
- Cold start: with few verified events an agent is calibrated by the global prior, which
  is weak until the fleet accrues data — hence shrinkage, not per-agent trust from n=1.
- v1 keys by agent, not agent×capability (the capability isn't on the task event yet), so
  a versatile agent's calibration is averaged across its capabilities.
- Isotonic on sparse data can overfit to step edges; PAVA + shrinkage mitigate, but the
  offline eval is what actually licenses turning it on.

**Neutral.**
- `bid_dispersion` (ROUTE-01) is the online health metric: calibration should *raise* it
  (more spread ⇒ more discrimination), the opposite of the uniform-overconfidence baseline.

## References

- ROUTE-05 (`docs/backlog/ROUTE-05-bid-calibration.md`); REPORT.md D4/R3;
  arXiv:2602.06948, arXiv:2601.07264 (LLM overconfidence). ADR-0002 (Gatekeeper),
  ADR-0011 (GatekeeperScore), ROUTE-01 (`bid_dispersion`).
