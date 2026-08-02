# ADR-0106: Commitments on the Epistemic Split (Knowledge Substrate Phase 2a)

**Status:** Implemented (gate 0/244 x3, backing default ON since 2026-08-01; see DECISIONS.md)
**Date:** 2026-08-01
**Relates to:** ADR-0105 (evidence foundation), ADR-0102 (drift record lane, premium),
the knowledge-substrate memo (§6, §8, §13, §18 are normative), DECISIONS.md 2026-08-01
(the settled §20 decisions, including the Phase 2a/2b split and the Finding collapse)

## Context

Phase 2 of the substrate programme moves the first real consumer — the drift lane's
commitments — onto the epistemic split, so that interpretations stop living in a private
`drift_commitments` table and start living as append-only knowledge items with a derived,
rebuildable belief projection. Per DECISIONS D-D, Phase 2a changes ONLY the storage and
derivation of commitments; the detector, reconciler, alert lane and trigger points are
untouched, which is what makes the drift-suite gate attributable.

## Decisions

### D1 — `knowledge_items` + typed `statement_values`, append-only, in OSS core

Migration `0012`. The envelope is narrow (memo §6): identity, kind, entity, attributed
actor + `asserted_at`, `source_ref`, negation, classification, validity, and NOTHING
technical — the technical envelope terminated at Evidence (ADR-0105). Values are typed
columns with an exactly-one CHECK; no JSONB payload, so the deferred `Event`/`Observation`
split stays additive. **No uniqueness exists on (entity, predicate, validity)** — two
contradictory items must coexist, because the disagreement is the signal (memo §8).

### D2 — Resolutions are a versioned projection; non-overlap lives ONLY there

One current row per `(namespace, kind, entity, actor, policy)` — enforced by a partial
unique index `WHERE system_to IS NULL`, the single place in the substrate where
non-overlap is allowed. A changed answer CLOSES the current version and inserts a new one,
so "what did we believe on date X" stays answerable.

### D3 — Order-independence is a pure function, not a convention

`domain.ResolveLatestAssertion(items)` computes the winner from the FULL item set with
total, deterministic ordering `(asserted_at, source_ref, negation)`. Every store derives
resolutions through it; nothing compares against "the prior row" (memo §13 — arrival
triggers computation, it must not define semantics). The permutation gate lives in
`domain/knowledge_test.go` and runs over every permutation, not a sample.

### D4 — The typed read boundary ships now, as `domain.KnowledgeStore` on the plugin seam

`PutItem` / `GetItem` / `CurrentResolutions`, exposed to plugins via
`app.KernelServices.Knowledge`. A plugin producing or consuming knowledge items goes
through this port — never SQL against substrate tables. Introducing the boundary after
consumers grew SQL dependencies would be the same retrofit the epistemic split itself
exists to avoid (memo §18 phase-order note).

### D5 — The drift lane's commitment backing becomes selectable, default unchanged

Premium `records.SubstrateCommitmentStore` implements the EXISTING
`records.CommitmentStore` + `CommitmentRetractor` seam over `domain.KnowledgeStore`:
supersession is not a write at all (the projection recomputes), retraction is a recorded
negation item, replay idempotency maps onto `(kind, entity, source_ref)`. Selection is the
premium env `CAMBRIAN_DRIFT_SUBSTRATE_COMMITMENTS=true` — the arm knob for the gate —
default off until the drift suite proves the backings score identically. The one
structural change on the old path: `reconcileExecutor.store` widened from `*PgStore` to
the interface, which was the single blocker to a second backing.

### D6 — What Phase 2a deliberately does NOT do (goes to 2b)

No outbox consumer; no `assessments`/`effects` tables (the reconciler's outcomes and
alerts stay on the drift lane — a table nothing writes is the unwired-subsystem failure
mode); `knowledge_items.evidence_id` remains nullable and unpopulated (the detector's lane
cannot see evidence ids); no `Finding` type (DECISIONS D-C: no human lifecycle state
exists anywhere to hold). Erasure (`DeleteByOwner`) still targets `drift_commitments`
only — under the substrate backing, erasure of knowledge items is an open Phase 2b item
and is recorded as such rather than half-implemented here.

## Gates

1. Drift suite: substrate arm vs same-binary control — recall, precision,
   deceptive-defeated and delta accuracy IDENTICAL.
2. Contradiction coexistence: two actors' conflicting statements both live
   (`TestSubstrate_ContradictorySpeakersCoexist`, plus the domain-level test).
3. Order independence: every permutation resolves identically
   (`TestResolveLatestAssertion_OrderIndependent`).
4. Supersession keeps every earlier item; retraction appends, never deletes.

## Addendum — Phase 2b (2026-08-01, same day; original text above unchanged)

The gate passed at row level (0/244 differing; DECISIONS.md), and Phase 2b
closed the D6 residuals that had real consumers:

- **Erasure parity.** `KnowledgeStore.EraseItems` (the substrate's ONE true
  deletion, compliance-only) + `SubstrateCommitmentStore.EraseByOwner` +
  `records.EraseOwner`, the composite a future erasure RPC must call.
  Substrate-first ordering is the correctness argument: the selector needs the
  owner's record keys and the SQL erasure deletes the rows they come from.
  A key that loses only some items re-derives; a key that loses all of them
  closes its current version with NO replacement. Caller status is exact
  parity with `DeleteByOwner`: tests today, the RPC when it arrives.
- **Write-cycle cost measured**: detect latency p50/p95 0/4 ms (SQL) vs
  0/6 ms (substrate) on the gate runs — ~2 ms at p95, same path-blind
  instrument caveat as the D6.3 entry.
- **Default FLIPPED**: `CAMBRIAN_DRIFT_SUBSTRATE_COMMITMENTS` now defaults ON;
  `false` is the explicit rollback lever. Confirmed on a fresh boot with no
  env var: treatment line present, drift suite again 0/244 vs control
  (`runs/20260801T113819Z_drift`).

**Deferred with reasoning, NOT silently dropped** (each would be wired to
nothing today, the repo's dominant failure mode): the evidence outbox consumer
(every current evidence row is produced by a lane that already fully processes
it inline; the consumer earns existence with the first non-inline source);
assessments/effects tables (no reader exists until a read surface or the
consumer lands); `evidence_id` linkage (the inline detector cannot see
evidence ids — at the outbox consumer the id is in hand and linkage is free,
whereas reconstructing thread-derived source keys premium-side is a hack
against the architecture).
