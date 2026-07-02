-- Migration 003: Replace importance_score with activation_strength + add scoring_prompt_version.
--
-- ADR-0015 (Engram Engine): activation_strength is the sole lifecycle metric for a memory.
--   - Default 0.1 (silent engram)
--   - Increases +0.05 per retrieval, capped at 0.8
--   - Decays nightly via Ebbinghaus stored procedure (pg_cron)
--   - Replace deprecated importance_score column
-- scoring_prompt_version stores the hash of the Tier-2 scoring prompt template used at commit time.
-- This enables future recalibration without retroactive inference.
--
-- Idempotent: safe to run twice (IF NOT EXISTS / IF EXISTS prevents errors on re-run).

ALTER TABLE documents ADD COLUMN IF NOT EXISTS activation_strength DOUBLE PRECISION NOT NULL DEFAULT 0.1;

ALTER TABLE documents ADD COLUMN IF NOT EXISTS scoring_prompt_version VARCHAR(8) NOT NULL DEFAULT '';

ALTER TABLE documents DROP COLUMN IF EXISTS importance_score;
