---
id: 0077
title: Gatekeeper Route-Preview — Deterministic Routing Data-Gen + Eval (ROUTE-07)
status: Accepted
date: 2026-07-17
supersedes: []
superseded_by: []
depends_on:
  - 0076-learned-gatekeeper-scorer
  - 0047-operator-transport-plane
  - 0002-gatekeeper-auction
---

# ADR-0077: Gatekeeper Route-Preview

## Status

Accepted — kernel side delivered (the `PreviewRoute` operator RPC + the pure `ScoreMerit`
core it exercises). The harness side (a `gatekeeper` benchmark suite + controlled synthetic
dataset that drive PreviewRoute to generate ROUTE-07 training data and score routing
precision) is the follow-on, built on this surface.

## Context

ROUTE-07 (ADR-0076) needs (a) *data* — labeled routing decisions to train the learned
scorer — and (b) *evaluation* — a measure of whether the Gatekeeper ranks the right agent.
Today the only way to produce a Gatekeeper funnel is the whole slow loop: planner LLM →
auction (agent bids) → agent execution → verifier. That is expensive, non-deterministic,
depends on a working (currently slow) planner LLM, and — the chicken-and-egg — needs
differentiated agent profiles that only exist after lots of prior execution. `plan_preview_only`
does not help: the auction (and thus the funnel) lives *inside* DAG execution, so a plan
preview yields no funnel.

## Decision

Expose the Gatekeeper's L3 merit scoring as a lean, deterministic **`PreviewRoute` operator
RPC** that scores a caller-supplied candidate set — no planner, no auction, no agent
execution — under the active scorer arm.

### `ScoreMerit`: the pure scoring core

`gatekeeper.computeMeritBreakdown` is refactored to fetch the profile then delegate to a new
**pure** `gatekeeper.ScoreMerit(profile, trait, requiredCaps, cfg, scorer) MeritBreakdown`
(behavior-preserving — the live path is unchanged, existing tests pass). Decoupling scoring
from profile *fetching* is what lets the benchmark score **inline synthetic profiles**
identically to the live decision — the same weights, the same ROUTE-06 tag scoping, the same
ROUTE-07 learned-scorer branch.

### `PreviewRoute` RPC (operator plane, contract 0055 → 0056, cap `route-preview`)

`PreviewRoute(task_desc, required_capabilities, [candidate profiles]) → ranked MeritResults +
arm`. The handler builds a `domain.AgentProfile` per inline candidate, runs `ScoreMerit`
under the active arm (`hand_weights` or, when `execution.learned_scorer` is on, the ROUTE-07
`learned_scorer`), ranks by score, and returns the funnel. It is **core, not premium** (the
Gatekeeper is core) and read-only — it scores, it never auctions or executes. Wired via an
`operator.RoutePreviewer` port (a thin `routePreviewAdapter` over `ScoreMerit`), the
established nil-in-OSS pattern.

### Why inline candidates (not the live fleet)

For a *controlled* benchmark the caller supplies synthetic profiles, so the eval is
deterministic and needs no seeded live fleet — it breaks the profile chicken-and-egg. A
future live-fleet mode (discover candidates + real profiles) is the organic-data variant.

## Consequences

**Positive.**
- Deterministic, planner-free, agent-free routing decisions — fast enough to generate
  thousands of labeled samples and to A/B hand-weights vs the learned scorer directly.
- The benchmark scores the SAME code path as production (`ScoreMerit`), for both arms.
- `ScoreMerit` extraction is a clean, reusable, behavior-preserving refactor.

**Negative / follow-on.**
- The harness side (re-vendor the operator stub to 0056, an `OperatorClient.preview_route`,
  the `gatekeeper` suite + controlled synthetic dataset of (task, candidates, gold-best-agent)
  rows, precision@k scoring, and feeding `route07-scorer extract` from its output) is not yet
  built — this ADR delivers the entry point it needs.
- Inline-candidate preview evaluates the *scorer*, not L1 declaration / L2 interview (those
  are candidate *filtering*, tested elsewhere).
- A second operator RPC + contract bump + ui/cli re-vendor debt (recorded as skew).

## References

- ROUTE-07 (`docs/backlog/ROUTE-07-learned-gatekeeper-scorer.md`), ADR-0076 (the learned
  scorer this benchmarks), ADR-0047 (operator plane). Code: `internal/supervision/gatekeeper/
  gatekeeper.go` (`ScoreMerit`), `internal/substrate/operator/route_preview.go`,
  `app/app.go` (`routePreviewAdapter`), `api/proto/operator.proto` (`PreviewRoute`).
