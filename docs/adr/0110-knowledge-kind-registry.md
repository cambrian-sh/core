# ADR-0110: The Knowledge Kind Registry (Knowledge Substrate Phase 6)

**Status:** Implemented (gate: fourth source by configuration alone, 2026-08-01; see DECISIONS.md)
**Date:** 2026-08-01
**Relates to:** ADR-0106 (items/values, latest_assertion), ADR-0108 (events/observations,
transformer seam), ADR-0109 (policy statements), the memo §5 (value types), §13
(authorities), §18 phase 6, §19.2 (the width gate)

## Context

Palantir's rule of three is satisfied by IMPLEMENTED behaviour, not speculation: three
real item producers exist (commitments, event observations, policy statements), each
privately enforcing the same three ideas — a constrained value per predicate, a
validation gate at the write, and a pure resolution function. Phase 6 extracts those
into declared machinery. The gate is the memo's: **a fourth source added by
configuration alone** — no new Go for the source.

## Decisions

### D1 — `domain.KindSpec`: a constrained type per PREDICATE, declared as data

`KindSpec{Kind, Policy, Predicates map[predicate]ValueSpec}` with
`ValueSpec{Type, Min, Max}` (range applies to numbers). Specs reach the kernel as
DATA: plugins contribute via `Registry.AddKnowledgeKinds` (add-many), deployments via
plugin configuration (JSON env). The kernel never learns what a predicate means —
only what shape it must have (memo §5: base types are domain-agnostic; value types
carry the constraint).

### D2 — Validation enforces at the typed boundaries, for DECLARED kinds only

`PgKnowledgeStore.PutItem` and `PgEventStore.RecordObservation` validate against the
registry: an undeclared predicate on a declared kind is REFUSED ("cannot express
safely" — never silently dropped, because a dropped value is an invisible data loss),
a type or range violation is refused with the constraint named. An UNDECLARED kind
passes untouched — adoption is monotonic and existing producers keep working until
they declare; that asymmetry is deliberate and recorded, not hidden.

### D3 — The authority interface is extracted from the one implemented resolver

`domain.ResolutionAuthority{Policy() string; Resolve(items) (winner, reason)}` —
a PURE function of the full candidate set (§13: no accumulator state, no
compare-to-prior). `latest_assertion` becomes the built-in implementation of the
interface it previously WAS. A kind's spec names its policy; the store resolves
through the registered authority for that policy and refuses to write items of a
declared kind whose policy nobody registered (a silent fallback would change belief
semantics without anyone deciding).

### D4 — The gate

Boot with a fourth source declared ONLY in configuration (a sensor-reading kind:
`temperature_c` number, range-bounded). Its traffic flows through the EXISTING
substrate lane; an in-range observation stores and answers a point lookup; an
out-of-range one is refused with the constraint named; and the drift suite scores
row-identical to control (the commitment kind is now declared and validated, and
declaring it must change nothing).

## Not in this phase

Cross-field validation, per-kind retention, authority CHAINING, and moving the drift
reconciler onto `ResolutionAuthority` (it is an assessment producer, not a resolution
policy; forcing it under this interface would blur the memo's assessment/resolution
split — it migrates with Phase 2b's assessments work, not here).
