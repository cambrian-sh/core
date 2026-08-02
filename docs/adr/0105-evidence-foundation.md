# ADR-0105: The Evidence Foundation (Knowledge Substrate Phase 1)

**Status:** Implemented (gates green + live-validated 2026-08-01; see DECISIONS.md)
**Date:** 2026-08-01
**Relates to:** ADR-0053 (general-purpose knowledge graph, deferred Layers 1–2), ADR-0104
(reactive-first architecture), ADR-0022 (content-addressed store), ADR-0095 (classification
inheritance), `docs/research/company-brain/knowledge-substrate-architecture.md` (the memo —
§6, §8, §10, §11 are normative for this ADR)

## Context

The knowledge-substrate programme (memo v4, two review rounds) replaces drift's private
commitment table with a shared temporal knowledge layer:

```
Evidence → KnowledgeItem → Assessment → Resolution → Effect
```

Phase 1 lands only the first stage: **preserve what arrived, immutably, before any semantic
processing** — so a failed extraction never loses the source material, and a later
re-extraction can always be run from the original bytes. Phase 0's measured baseline exists
(DECISIONS.md 2026-08-01, SUB-00): zero leaks, filtered vector retrieval exact at current
scale; no storage decision here rests on an unmeasured assumption.

Only two things blocked starting, per the memo: the blob/database commit ordering (§10) and
the decomposed replay guarantees (§11). Both are what this ADR implements. Everything else
blocks *freezing the schema*, not beginning.

## Decisions

### D1 — Evidence is an OSS-core domain concept, named generically

`domain.Evidence` + `domain.EvidenceStore` live in `cambrian-core`. Nothing in the type,
the schema or the comments names drift, commitments, or any premium feature (open-core
boundary, ADR-0057). Authorities, extractors and drift-as-first-consumer arrive in premium
via the existing add-many registry in later phases.

### D2 — Content-first commit ordering

The write path is, in order, and the order is the contract:

1. **Put the original bytes** into the existing `domain.ContentStore` (ADR-0022) —
   idempotent under the content hash; a retry after a crash re-puts the same CID.
2. **Verify** the content is retrievable under that CID.
3. **Atomically insert the evidence row and its outbox work item** in one transaction,
   referencing the CID.
4. Only then may the caller **acknowledge the sender**. A webhook ACKed earlier can lose
   data the sender believes was delivered.
5. Orphan blobs (crash between 1 and 3) are garbage, not damage — `ContentStore.GC`
   already exists to collect them.

This deliberately trades harmless orphan blobs for the impossible alternative: an evidence
row whose source material cannot be reprocessed, which is a silent, permanent hole in the
archive (memo §10; the v3 draft had this bug inverted).

### D3 — Idempotency is decomposed, never claimed as end-to-end exactly-once

| Layer | Guarantee | Mechanism |
|---|---|---|
| Evidence insertion | Idempotent | `UNIQUE (namespace_id, source_id, source_key, source_revision)`; a replay returns the existing row's identity and reports `inserted=false`. It never creates a duplicate version |
| Outbox transition | Exactly once, logically | `UPDATE … SET processed_at = now() WHERE id = $1 AND processed_at IS NULL` — the row count is the truth; a second consumer observes 0 rows |
| Work delivery | At least once | The outbox is scanned by a future consumer (Phase 2); crash after evidence commit and before consumption replays the item |

### D4 — Source revisions are new evidence, never an update

An evidence row is immutable. A changed source artifact arrives as a **new row** with a
higher `source_revision`, linked by `revises_id`. `UPDATE` on evidence is not in the port's
vocabulary at all, so "what did we receive, and when" stays answerable forever (memo §8).

### D5 — `namespace_id` from day one, constant-valued

Every table and every unique index carries `namespace_id` (default `'default'`). One
instance per customer makes multi-tenancy moot today; retrofitting the column across
evidence, items, resolutions and every unique index later is the expensive path. The name
is "namespace", not "tenant", deliberately (memo §6).

### D6 — Capture wires into the ONE existing ingest chokepoint, flag-gated, default off

`IngestionManager.ProcessSync` is where every door already funnels (agent plane, operator
plane, reactive plugins — ADR-0104 D3 made that sameness the point). Evidence capture is a
seam at the top of it: when enabled and constructed, the document's original bytes become
evidence *before* chunking/embedding; when the capture step fails, ingest fails — a lane
that accepted content and cannot preserve it must not pretend it did.

Flag: `execution.ingestion.evidence_capture_enabled`, default **false**. Default-on is a
behavior change for every deployment's ingest path and belongs to a later ADR once Phase 2
gives evidence a consumer. A flag parsed but unthreaded is the known config trap; the
consumer is wired in the same change as the field.

### D7 — What Phase 1 deliberately does NOT build

No `KnowledgeItem`, no `statement_values`, no assessments, no authorities, no typed read
boundary, no retention/partitioning, no object storage backend (the ContentStore port makes
that a backend swap later). Those are Phases 2+ and are recorded in the memo's staging
table. The God Object risk remains *contained, not fixed*, until `Event`/`Observation`
storage exists — that closure belongs to the phase that adds a second event-shaped source.

## Gates (definition of done, from the memo's §17 crash table)

- Crash after blob write, before DB commit → orphan blob may remain; **no published
  evidence row**.
- Crash after evidence commit, before outbox consumption → work replays; **exactly one
  logical transition**.
- Replay of an identical (source, key, revision) → **no duplicate evidence version**.
- A deliberately failed extraction still leaves evidence that can be reprocessed.

The ordering gates are enforced by construction (the ingestor calls the store only with a
verified CID in hand) and asserted by failpoint tests; the idempotency gates are asserted
against the real SQL semantics.

## Consequences

- Migration `0011_evidence.sql` (additive; rollback is `DROP TABLE evidence_outbox, evidence`).
- The ingest hot path gains one ContentStore put + one INSERT per document when the flag is
  on; nothing when off. No LLM calls anywhere in the lane (hard rule).
- The outbox has no consumer yet. That is Phase 2's first line of work, and the unconsumed
  backlog is bounded by the flag being default-off until then.
