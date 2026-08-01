-- 0013_chunk_embeddings.sql — embeddings as a versioned projection (ADR-0107 D1,
-- Knowledge Substrate Phase 3, stage 3a).
--
-- `chunks.embedding` is a vector(1024) COLUMN, which is why an embedding-model
-- change cannot run two models side by side and why the only migration path was
-- a destructive re-embed. This table is the projection that ends that: one row
-- per (chunk, model), UNTYPED vector so any dimension fits, rebuildable and
-- detachable without touching the row it serves.
--
-- Deliberately NO HNSW index here (ADR-0107 D2): an untyped vector column takes
-- a per-model PARTIAL index over a cast — e.g.
--   CREATE INDEX ... USING hnsw ((embedding::vector(1024)) vector_cosine_ops)
--     WHERE model_id = 'bge-large';
-- and the model set is deployment DATA, so that DDL belongs to the backfill
-- tool (stage 3b/3c), not to a migration. Stage 3a only ever writes.
--
-- ADDITIVE: chunks.embedding is not dropped, renamed or rewritten; it remains
-- the serving path until a cutover decision with its own DECISIONS entry.
-- Rollback: DROP TABLE chunk_embeddings;

CREATE TABLE IF NOT EXISTS chunk_embeddings (
	chunk_id      TEXT NOT NULL REFERENCES chunks(id) ON DELETE CASCADE,
	model_id      TEXT NOT NULL,
	model_version TEXT NOT NULL DEFAULT '',
	dims          INT  NOT NULL,
	embedding     vector NOT NULL,
	created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (chunk_id, model_id)
);
