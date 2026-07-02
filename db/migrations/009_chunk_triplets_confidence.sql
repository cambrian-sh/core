-- Migration 009: per-triple confidence + provenance on chunk_triplets
-- (ADR-0053 D2, revised 2026-06-25 — tiered extractor).
--
-- D2 originally produced every triplet with one LLM call per chunk. The frozen
-- kg_extractors/ tiered pipeline (metadata + spacy_patterns + LLM-residue,
-- see kg_extractors/WHAT_TO_DO.md §7) produces triplets from multiple sources,
-- so each row now records:
--
--   * confidence — the agreement tier, driving the routing in WHAT_TO_DO §1
--       2 = high   : >=2 tiers agreed, or a high-precision deterministic pattern
--       1 = low    : a single tier produced it
--       0 = filler : produced only by the LLM residue tier
--       NULL       : legacy rows (pre-009, all from the old LLM batcher) — readers
--                    treat NULL as filler. (Acceptance criterion: existing rows stay
--                    valid with confidence=NULL.)
--   * sources    — which producers emitted it, any subset of
--                    {metadata, spacy_patterns, llm}. Auditable + usable as
--                    training labels for a future ensemble.
--
-- The routing layer (ADR-0053 D3 / WHAT_TO_DO §1) reads confidence: high → answer
-- path directly, low → LLM-residue confirmation. The KG primitive (per-chunk
-- (h,r,t)) and the KG²RAG retrieval path are unchanged.
--
-- Mirrors the inline ADD COLUMN in PgVectorAdapter.ensureSchema (boot-time
-- safety net); this file is the canonical, ordered migration record.
--
-- Idempotent: safe to run twice (ADD COLUMN IF NOT EXISTS; the backfill only
-- touches rows whose sources is still NULL).

ALTER TABLE chunk_triplets ADD COLUMN IF NOT EXISTS confidence SMALLINT;
ALTER TABLE chunk_triplets ADD COLUMN IF NOT EXISTS sources    TEXT[];

-- Backfill: every pre-existing row was produced by the old single-LLM batcher.
-- Stamp provenance ({llm}); leave confidence NULL so readers treat them as filler
-- until they are re-extracted by the tiered pipeline.
UPDATE chunk_triplets SET sources = ARRAY['llm']::TEXT[] WHERE sources IS NULL;

-- A GIN index makes "triplets from source X" / "triplets the rule stack produced"
-- queries cheap (used by the agreement-oracle and training-export paths).
CREATE INDEX IF NOT EXISTS idx_chunk_triplets_sources ON chunk_triplets USING GIN (sources);
