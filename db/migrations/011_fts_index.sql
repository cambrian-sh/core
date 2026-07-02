-- Migration 011: full-text (BM25-ish) index for hybrid retrieval (ADR-0054).
--
-- The sparse half of hybrid dense+lexical search. An EXPRESSION GIN index over
-- to_tsvector('english', text) — no stored column, no backfill of existing rows
-- (the index computes the tsvector). Lexical search matches with
--   to_tsvector('english', text) @@ <tsquery>
-- and ranks with ts_rank_cd, which uses this index.
--
-- Catches exact-token chunks (names, titles like "Charlotte's Web", places like
-- "Sweden") that the dense embedder ranks low. Fused with the vector results via
-- Reciprocal Rank Fusion in the recall path.
--
-- Mirrors the inline CREATE INDEX in PgVectorAdapter.ensureSchema. Idempotent.

CREATE INDEX IF NOT EXISTS idx_doc_fts ON documents USING gin (to_tsvector('english', text));
