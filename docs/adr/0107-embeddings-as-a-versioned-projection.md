# ADR-0107: Embeddings as a Versioned Projection (Knowledge Substrate Phase 3)

**Status:** Proposed — design accepted for staged implementation; no code yet
**Date:** 2026-08-01
**Relates to:** ADR-0105/0106 (the substrate so far), ADR-0093 (documents/chunks split),
the knowledge-substrate memo §9 ("Embeddings are a projection, not a column"),
`cambrian-debugging-playbook` (the destructive dim-migration scar)

## Context

`chunks.embedding` is a `vector(1024)` COLUMN. The memo names the consequence and this
deployment has lived it: **an embedding-model change cannot run two models side by side
when the vector is a column** — the only path is `ALLOW_DESTRUCTIVE_DIM_MIGRATION=1` on
an expendable store, which is exactly the "silent, permanent" class of operation the
substrate exists to forbid. Embeddings have a different lifecycle from the rows they
serve: they change when the model changes, and they must be rebuildable and comparable
across models before a cutover (memo §17: "two embedding versions coexist — search
comparable before cutover").

Phase 3's other half — evidence-citing retrieval results — is deliberately out of this
ADR: document↔evidence linkage already exists by construction (`evidence.source_key` is
the same `externalDocumentID` the document entity carries), and surfacing it is a small,
separate change once this lands.

## Decisions

### D1 — An additive projection table, not a column migration

Migration `0013`:

```sql
CREATE TABLE chunk_embeddings (
  chunk_id      TEXT NOT NULL REFERENCES chunks(id) ON DELETE CASCADE,
  model_id      TEXT NOT NULL,     -- "bge-large"
  model_version TEXT NOT NULL DEFAULT '',
  dims          INT  NOT NULL,
  embedding     vector NOT NULL,   -- UNTYPED: any dimension; typing happens at the index
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (chunk_id, model_id)
);
```

`chunks.embedding` is NOT dropped, renamed, or rewritten (the additive rule). It remains
the serving path until a cutover decision with its own DECISIONS entry.

### D2 — Per-model partial HNSW indexes over a cast expression

An untyped `vector` column cannot carry one HNSW index (the index needs fixed
dimensions). Each model gets a partial expression index:

```sql
CREATE INDEX chunk_embeddings_bge_large_hnsw ON chunk_embeddings
  USING hnsw ((embedding::vector(1024)) vector_cosine_ops)
  WHERE model_id = 'bge-large';
```

Index DDL for a NEW model is issued by the backfill tool (D4), not by migrations — the
model set is deployment data, not schema. Queries must repeat the cast and the predicate
verbatim or the planner falls back to exact scan (correct, slower — the same fail-safe
shape Phase 0 measured).

### D3 — Reads switch by configuration; dual-write is flag-gated

- `execution.retrieval.embedding_projection_write` (default false): every embedding
  write ALSO lands in the projection under the active embedder's `(model_id, dims)`.
- `execution.retrieval.embedding_projection_read` (default false): dense retrieval
  reads the projection for the ACTIVE model instead of `chunks.embedding`.
- The active model is what `embedder.json` already declares; no new identity is coined.
  `ExecutionConfig.RetrievalFingerprint` (ADR-0103) must incorporate the read flag +
  model id, so receipts can attribute a ranking to the projection that produced it.

Write flag first, read flag later, cutover last — three separately measurable arms.

### D4 — Rebuild is a first-class tool, and its time is a recorded number

`cmd/embed-backfill`: stream `chunks` (id, text), embed under a NAMED model, upsert
projection rows, create the model's index if absent. Idempotent (`ON CONFLICT DO
UPDATE` only when `model_version` changed). The wall-clock for a full rebuild is the
"projection rebuild RTO" row of the memo's §17 table and goes in DECISIONS.md with the
run.

### D5 — Gates (each stage has one)

| Stage | Arm | Gate |
|---|---|---|
| 3a: migration + dual-write | `embed-projection-write` vs control | Ingest latency delta recorded; retrieval EXACTLY unchanged (reads untouched) |
| 3b: read switch, same model | `embed-projection-read` vs control | recall@k and suite scores IDENTICAL (same vectors, different table); p95 latency recorded |
| 3c: second model coexists | backfill a second model, switch, switch back | Both models queryable through the switch with no writes lost; comparison runs recorded; rebuild RTO recorded |

The failure archaeology's warning binds here harder than anywhere: this is the hottest
path in the product, and every stage's arm must show ZERO retrieval-quality movement
before the next stage starts.

## Rejected

- **Dropping/retyping `chunks.embedding` in place** — the destructive migration this
  ADR exists to end.
- **One projection table per dimension** (`chunk_embeddings_768`, …) — schema churn per
  model; the partial-index-over-cast pattern achieves the same plans without DDL-per-dim
  table proliferation.
- **Vector columns per model** — reintroduces the column lifecycle problem N times.
