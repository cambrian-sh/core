-- Migration 008: Add the chunk_triplets table (ADR-0053 Phase 0).
--
-- chunk_triplets is the per-chunk (h, r, t) KG that powers one-hop KG²RAG
-- chunk expansion at recall time. The KG²RAG model: the knowledge graph
-- lives at the chunk level; each row is one triplet the LLM observed in
-- the chunk's text. The retrieval path walks triplets from the seed
-- chunks, collects referenced entities, and pulls in chunks that share
-- those entities.
--
-- Design choices:
--   * chunk_id is a TEXT (not an FK) because the producer/owner document
--     is identified by `documents.id` which is also TEXT. We omit the FK
--     on purpose: orphans are tolerable (KG expansion just skips them),
--     and a strict FK would force CASCADE behavior we don't want here
--     (deleting a document should NOT delete the historical fact that
--     it once had a triplet — we may want to re-link it).
--   * h and t are free-form text (canonicalized to lowercase on insert
--     by the LLM extractor). They are NOT a closed vocabulary — entity
--     names emerge from the data, like KG²RAG itself.
--   * r is the free-form verb phrase ("researched", "born in", ...).
--   * weight defaults to 1.0 (LLM extractor is binary: extracted or not).
--   * PK (chunk_id, h, r, t) makes the insert idempotent — repeated
--     extraction of the same chunk yields the same triplets, no dupes.
--   * Indexes on h and t (separately) so the entity->chunks lookup that
--     powers KG expansion is O(log n) per entity.
--
-- Mirrors the inline CREATE in PgVectorAdapter.ensureSchema
-- (boot-time safety net); this file is the canonical, ordered migration
-- record.
--
-- Idempotent: safe to run twice (CREATE TABLE / INDEX IF NOT EXISTS).

CREATE TABLE IF NOT EXISTS chunk_triplets (
    chunk_id     TEXT NOT NULL,
    h            TEXT NOT NULL,
    r            TEXT NOT NULL,
    t            TEXT NOT NULL,
    weight       REAL NOT NULL DEFAULT 1.0,
    extracted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (chunk_id, h, r, t)
);

CREATE INDEX IF NOT EXISTS idx_chunk_triplets_h ON chunk_triplets (h);
CREATE INDEX IF NOT EXISTS idx_chunk_triplets_t ON chunk_triplets (t);
