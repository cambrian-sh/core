# ADR-0108: Event-Shaped Knowledge (Knowledge Substrate Phase 4)

**Status:** Proposed
**Date:** 2026-08-01
**Relates to:** ADR-0105 (evidence + outbox), ADR-0106 (epistemic split), the
knowledge-substrate memo (§6, §7A/B/E, §15, §18 phase 4), DECISIONS.md 2026-08-01
(the Phase-2b deferrals this phase un-defers)

## Context

Every deferred piece of the substrate was waiting on the same trigger: a source whose
deliveries are EVENTS (a reading, a passage, a transfer) rather than prose. Phase 4 is
that trigger, owner-directed. It lands the deferred sub-types — `Event` + `EventRole`
(n-ary occurrences with traversable roles, §7B) and `Observation` (high-volume
entity/predicate/value-at-time rows, §7A/E) — which is what the memo's scorecard (§15)
requires before the God Object risk counts as CLOSED rather than contained. It also
gives the ADR-0105 outbox its first real consumer: an event-shaped delivery has no
inline processing lane, so transformation genuinely begins after evidence here.

## Decisions

### D1 — Storage (migration `0014`): three additive tables, observations partitioned

- `events` (id, namespace, event_type, occurred_at, evidence_id, source_ref) with
  `event_roles` (event_id, role, entity_id) — a role is a ROW with an indexable
  entity edge, never a key inside a JSON value (the §7B bug).
- `observations` (namespace, entity_id, predicate, value_* typed exactly-one,
  location, occurred_at, confidence, evidence_id, source_ref) — **PARTITION BY RANGE
  (occurred_at)** with a DEFAULT partition, so partitioning is concrete now and adding
  monthly partitions later is DDL, not redesign. A raw sample is a row HERE and does
  not automatically become a KnowledgeItem/Assessment/Resolution (§7E's corrected rule).
- Idempotency mirrors evidence: unique on (namespace, source_ref) where present.
  Nothing here is embedded, chunked, or touched by any model — the gate depends on it.

### D2 — The typed reads are SQL, and the guarantee is stated per §14

`domain.EventStore`: `RecordEvent`, `RecordObservation`, `PointLookup(entity,
predicate)` → latest stored observation, `History(entity, predicate, from, to)` →
range scan. Exact over stored data; identity epistemically uncertain; never "truth".

### D3 — The outbox consumer exists now, as a kernel lifecycle

`internal/evidence.Consumer`: polls `PendingOutbox`, hands each evidence row (with its
bytes from the ContentStore) to every registered `domain.EvidenceTransformer`
(add-many via `app.Registry.AddEvidenceTransformer`), then `MarkProcessed` — the
exactly-once-logical transition ADR-0105 D3 already pinned. A transformer error leaves
the item pending (at-least-once; transformers must be replay-safe, which D1's
idempotent writes make structural). Runs only when evidence capture is enabled AND at
least one transformer is registered — a consumer with no consumers is the unwired trap
in a hat.

### D4 — Transformation and the wire surface are PREMIUM; the mapping is data-shaped

The event transformer (thin-envelope JSON → Event/Observation rows) and the
benchmark-facing gRPC plane (`SubstrateLane`: `IngestEvent`, `PointLookup`,
`History`) live in `cambrian-premium`, mounted via `Registry.AddGRPCService` exactly
like the receipts lane — **no operator-contract bump**. The envelope is the §19.5 thin
contract (stream, timestamps, entity, payload); the kernel never learns a domain
vocabulary. Ports reach premium through `KernelServices` (`EvidenceIngest`, `Events`),
the ADR-0106 D4 pattern.

### D5 — Gate (memo §18 phase 4)

Point lookup and history answered correctly **with nothing embedded**: after ingesting
an event corpus, `chunks` and `chunk_embeddings` row counts are UNCHANGED; replaying a
delivery creates no duplicate event/observation; the answers are exact over stored
rows through the async evidence→outbox→transformer path.

## Not in this phase

Retention sweeps for observation partitions (the partition layout is the enabler;
the sweep is config surface for a later slice); `Relation`/`Derivation` types;
promotion rules (threshold authority) from observations to knowledge items — that is
the §7E "promoted samples" lane and deserves its own arm.
