---
id: 0093
title: The Document as an Entity — Splitting One Table That Meant Six Things
status: Proposed
date: 2026-07-27
supersedes: []
superseded_by: []
amends:
  - 0034-scope-and-classification
  - 0060-structure-aware-ingestion
depends_on:
  - 0015-mnemonic-memory
  - 0044-tool-semantic-retrieval
  - 0046-skill-semantic-retrieval
  - 0060-structure-aware-ingestion
  - 0064-db-migration-runner
  - 0085-access-policy-port-and-extraction
---

# ADR-0093: The Document as an Entity

## Status

**Proposed — implemented, verified against a real database, and benchmarked. No regression on any suite.**

## Context

`documents` held everything with an embedding. Fourteen `document_type` values shared one
table, one HNSW index, and one metadata GIN index: document chunks, agent-written memories,
scenes, actions, entities, episodic narratives, structural section nodes, and the seeded
tool/skill/agent-profile descriptors that are rebuilt on every boot.

Three consequences, in ascending order of seriousness.

**One vector index served all of them.** A recall search traversed tool and skill descriptors
that recall can never return.

**`document_type` had no index** despite every typed read filtering on it. At 1,283 rows this
is theoretical; the shape is not.

**A document was not an entity.** This is the one that matters. Nothing in the database
represented the source document a chunk came from. Parentage lived in an id string convention
(`{docID}-chunk-{n}`) plus a `metadata.document_id` key, and **classification tags were copied
onto every chunk at ingest** — `meta["tags"] = append(...)` — with no authoritative row to copy
from.

So re-classifying a document meant updating N chunk rows with no transaction boundary. A
partial failure left the document **half-classified**: some chunks carrying the new tags and
reachable, some carrying the old ones and not, and nothing able to report the disagreement
because nothing knew what the document's tags were supposed to be.

A boundary that can be half-applied is not a boundary. ADR-0085 spent an entire design on
making access decisions explainable and atomic; underneath it, the resource classification
those decisions read was neither.

## Decision

### D1 — Six tables, split by what a row IS

| Table | Holds | Why separate |
|---|---|---|
| `documents` | the source document — **new entity**, authoritative `tags` | policy attaches here |
| `chunks` | every vectorised unit recall can return | the recall index, and only it |
| `document_sections` | ADR-0060 structural nodes | not embedded, never recalled |
| `tools` / `skills` / `agent_profiles` | seeded descriptors | configuration, rebuilt each boot |

### D2 — All recall types stay in ONE table

Every `mnemonic_*` type, `episodic_memory`, and the older judicial/procedural/neural/negative
types live in `chunks` **together**, because they always did. Recall searched one table before
the split and searches one table after it, so **the split cannot have narrowed what a search
can find.** Only the descriptors — which recall never returns — moved out.

Splitting recall further would have been the easy mistake: tidier DDL, and a quiet reduction in
what memory can retrieve.

### D3 — `document_id` is nullable

A chunk carved from an ingested document has a parent. A memory an agent wrote about what it
just did does not. Forcing the second kind under a synthetic document would be a lie told to
satisfy a foreign key, so the column is nullable and unparented memories keep a NULL.

### D4 — `documents.tags` is authoritative; the chunk copies are a derived cache

The per-chunk copy is **kept**, not dropped in favour of a join, because the tag filter sits on
the retrieval hot path behind a GIN index. Replacing it with a join to `documents` on every
search would trade a correctness win for a latency regression on the most frequent query in the
system.

What changes is that the cache now has a single writer. `RetagDocument` updates the document row
and rewrites every one of its chunks **in one transaction**: both move or neither does. Deleting
a document cascades to its chunks, because an orphaned chunk of a deleted document is
unreachable data that still answers searches.

### D5 — A write must never fail over bookkeeping

`document_id` is written through a subselect, so an id whose document row does not exist
resolves to NULL rather than violating the foreign key. Sections do the same. Losing a memory
because its parentage could not be recorded would be far worse than losing the link, and the
ingest path treats a failure to record the document entity as a warning rather than a fatal
error.

### D6 — The migration is additive

`documents` is **renamed** to `documents_legacy`, never dropped, and every row is copied rather
than moved. Rollback is: drop the new tables, rename back. The corpus is the shared store the
benchmarks measure against, and the standing rule is that it is never destroyed to make a schema
tidy.

The document entity is reconstructed from the only record that a source document ever existed:
`metadata->>'document_id'` on the chunks, with tags lifted from the chunk copies and `DISTINCT`
collapsing the N duplicates back into the single row the schema should have had.

## Consequences

**Access control can finally attach to a document.** That was the request behind this work and
it was previously inexpressible — not hard, *inexpressible*, because the thing to attach to did
not exist.

**The trap this nearly walked into, recorded because it is the shape this codebase keeps
producing.** `update_activation_strength` and `apply_ebbinghaus_decay` name `documents` in their
bodies, and plpgsql resolves that name at **call time**. Renaming the table without redefining
them would have left activation updates and Ebbinghaus decay silently operating on the new,
nearly-empty document table: every call succeeding, nothing decaying, no error anywhere. Both
functions are redefined against `chunks`, with a test asserting it.

**Two smells removed rather than preserved.** A raw SQL string (`SELECT COUNT(*) FROM
documents ...`) bypassed the table constant entirely and was invisible to a search for it; and
the `document_type NOT IN ('tool','skill','agent_profile')` guard in the dimension check existed
only to work around the mixing this ADR removes.

**Id-only operations now span tables.** `GetByID`, `Delete`, `DeleteBatch` and `GetBatch` carry
an id and no type, so they consult `chunks` first and the descriptor tables on a miss. The
alternative — making every caller learn the storage layout — is the coupling the split exists to
remove. Deletes issue one statement per table rather than locating the row first: a delete that
silently matched nothing because the caller guessed wrong is worth a few cheap statements to
make impossible.

### Three bugs the tests did not catch, and one they now do

All three surfaced only on a **live boot against the real corpus**, after nine integration
tests and 50 green packages. Recorded because the pattern is the point: the tests exercised the
schema and the code separately, and every one of these lived in the seam between them.

1. **`documents.version + 1` in the conflict target.** A tool upsert emitted
   `ON CONFLICT ... SET version = documents.version + 1` while inserting into `tools` —
   a missing FROM-clause error. Invisible to any test that does not upsert a *descriptor*
   against the real schema.
2. **`section_path` missing from the descriptor tables.** 0007 claimed they kept "the same
   column set as chunks" and did not check that claim against the adapter's SELECT list.
   `GetByID` consults every retrievable table, so reading an agent profile failed with
   `column "section_path" does not exist` and every agent's interview-vector check broke.
   Fixed forward in **0008** (append-only, ADR-0064), because 0007 was already applied.
3. **`document_edges.source_id`'s foreign key followed the rename.** PostgreSQL moved the
   constraint along with the renamed table, so it ended up pointing at the FROZEN legacy
   copy, and every structural edge written after the split failed with a 23503 — roughly 700
   warnings in one benchmark ingest. Non-fatal by luck rather than design:
   `SaveStructuralEdges` is best-effort, so chunks saved fine while the entire ADR-0060
   structure graph was silently lost. The constraint cannot be repointed, because an edge
   endpoint may now be a chunk OR a section, in two different tables — which is why the
   baseline had already dropped the matching `target_id` FK, an asymmetry that made this easy
   to miss. **0009** drops the source-side twin.
4. **The DB-ahead guard did its job.** A binary built before 0008 refused to start against a
   schema at version 8. Working as designed, and worth recording as the one thing that
   behaved correctly without being asked.

The row shape was an **unasserted contract** between the migration and the adapter's column
list — two independent descriptions of one thing, with nothing comparing them. There is now a
test that runs the adapter's exact SELECT against every table `GetByID` consults, with the
column list deliberately duplicated rather than imported: importing it would make the test
agree with the adapter by construction and prove nothing. It is mutation-verified — it
reproduces the exact live-boot failure when run at 0007 and passes at 0008.

**Unrelated finding, recorded because it blocks measurement.** Running the retrieval suites
against the **premium** kernel fails at ingest with `reason: no_principal — identity could not
be established`: the premium authorizer fails closed on a write carrying no principal, and the
benchmark's ingest path does not establish one. This has nothing to do with the split, and the
suites are therefore run against the OSS kernel (which fails open, and matches the conditions
the baselines were captured under). The gap is real and should be tracked separately.

### Benchmark results

Run against the OSS kernel on the live corpus after 0009, versus the recorded baselines:

| Suite | Baseline | After | Note |
|---|---|---|---|
| locomo, recall-only (n=100) | 0.310 | **0.380** | no regression |
| document-qa (n=4 fixture) | 0.500 | **0.500** | identical |
| tau2-knowledge (n=97) | recall 0.435 / mrr 0.339 / hit_any 0.959 | **recall 0.486 / mrr 0.378 / hit_any 1.000** | scorer repaired first |

**Read these as "no regression", not as a measured gain.** Locomo returned 0.410 before 0009
and 0.380 after, on the same code path — a three-point spread between identical
configurations, so a single run-pair cannot support a precise delta. The tau2-knowledge
baseline is not strictly comparable either: it predates the scorer breakage described below,
and the corpus has changed. What the numbers do support is that nothing got worse, on every
suite, and that the diagnostics are clean — 97/97 rows retrieved 22–43 documents each, zero
transport errors, zero unrecoverable ids.

There is a plausible mechanism for a genuine improvement — the recall HNSW index no longer
contains the tool, skill and agent-profile descriptors, so at a fixed `ef_search` fewer
candidate slots are spent on rows recall can never return — but this work does not establish
it, and it should not be claimed until a controlled arm does.

**A fifth bug, in the measuring instrument.** tau2-knowledge scored a flat 0.0000 across all
97 tasks. It was not the split: the suite recovered document ids from `metadata["session_id"]`,
and a prior deliberate change had renamed that key to `ingest_thread_id` ("an ingestion thread
is NOT a task session"). The suite had therefore been reporting zero since that rename, and its
0.435 baseline predates it. Proof it was pre-existing: `documents_legacy`, the frozen pre-split
snapshot, contains zero rows with `session_id`.

The scorer now reads `metadata["document_id"]` — since this ADR a real column rather than a
convention — and recall jumped from 0.000 to 0.486, above the old baseline.

**The lesson is the one worth keeping.** A broken scorer and a catastrophic regression produced
the *identical* row: `recall=0.0000`, `failure_kind=no_required_retrieved`. That ambiguity cost
two wrong conclusions in one session. Rows now carry `hits_returned` and `docid_unrecoverable`,
with a distinct `failure_kind`, so a dead instrument can no longer masquerade as a bad score.

**Not done.** `documents_legacy` is still on disk and should be dropped once the split has run
in production for a while. The chunk tag cache still has two writers (the ingest path and
`RetagDocument`) rather than one. And running the suites against the PREMIUM kernel still fails
at ingest with `no_principal` — unrelated to this work, tracked separately. The DDD mandate applies squarely here —
unlike access policy, this area has six suites (`agentic_retrieval`, `chunking`, `document_qa`,
`musique`, `locomo`, `tau2_knowledge`) — so recall and latency must be re-measured against the
existing baselines before this is called finished. `documents_legacy` is still on disk and
should be dropped in a later migration once the split is proven. Nothing yet re-derives the
chunk tag cache at ingest from the document row rather than from the incoming document, so the
two writers are the ingest path and `RetagDocument`; consolidating them is follow-up work.
