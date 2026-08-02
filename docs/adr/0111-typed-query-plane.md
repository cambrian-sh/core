# ADR-0111: The Typed Query Plane (Knowledge Substrate Phase 7)

**Status:** Implemented (gate 10/10 live 2026-08-01; see DECISIONS.md)
**Date:** 2026-08-01
**Relates to:** ADR-0106/0108/0110 (the stores and registry it reads), the memo §14
(the seven question shapes and the guarantee ladder) and §18 phase 7

## Context

The memo's end-state access shape: a CLOSED, validated AST — never text-to-SQL — where
"a model may form the AST, but an unexpressible request returns 'cannot express
safely', never invented SQL" (§14, the Palantir Object-Query-Tool transplant). Six of
the seven question shapes now have typed rows to answer from; the seventh
(open-ended) is DELIBERATELY excluded: it belongs to the corpus/memory lane, and the
two-tier split is the design, not a gap.

## Decisions

### D1 — One closed query struct, validated before anything executes

`domain.KnowledgeQuery{Kind, EntityID, Predicate, ItemKind, Policy, Actor, AsOf,
From/To, Aggregate, Hops, Limit, EvidenceID}` with Kind from a closed set:
`point · history · as_of · current · contradictions · aggregate · events · traverse ·
evidence`. `Validate()` refuses everything outside the set — unknown kind, unknown
aggregate function, unbounded traversal (hops capped), missing required fields —
wrapping `domain.ErrCannotExpress` with the reason NAMED. Validation runs before any
store is touched: an invalid query never half-executes.

### D2 — Execution composes the existing typed stores; new SQL only for new shapes

`postgres.PgQueryPlane` reuses `EventStore` (point/history/events),
`KnowledgeStore` (current), `EvidenceStore` (evidence inspection) and adds exactly
four read shapes: as-of over the resolutions version history (`system_from`/`system_to`
— the §14 row that is exact AND complete, a question about our own records),
contradictions (current resolutions of one entity disagreeing across actors — the
disagreement surfaced, never resolved away), bounded aggregates over observations
(count/avg/min/max on the number column), and bounded traversal over event roles
(entity → events → co-participants, visited-set, hop-capped). No specialised
infrastructure: the memo's rule is a measured failure class first, and none exists.

### D3 — The wire surface is the premium lane; rows are JSON objects in v1

`SubstrateLane.Query` mirrors the AST field-for-field; results return as JSON-encoded
rows (a closed INPUT is the safety property — the output shape can harden into typed
messages when a consumer needs it, recorded as a v1 limit). `ErrCannotExpress` maps
to `InvalidArgument` with the reason, so a caller can distinguish "unexpressible"
from "empty" — the same refusal/empty separation the access-policy work bought.

### D4 — Guarantees ride the answer

Every response carries the §14 guarantee label for its kind (exact-over-stored /
exact-and-complete for as-of / deterministic) so no caller can quote a point lookup
as world truth without deleting words the API handed them.

## Gate

All SEVEN §14 shapes answered through the plane against live data (open-ended
explicitly answered by refusal, naming the corpus lane as the right tool), and every
malformed/out-of-bounds query refused with "cannot express safely" — refusal, never a
guess, never invented SQL.
