-- 0065_evidence_derived_from.sql — link evidence a pipeline node fetched to the
-- evidence that caused the fetch (T-20261114-pipeline-fetch-archives-fetched-bodies-with-no,
-- ENG-1611; owner decision 2026-11-19).
-- cambrian:not-destructive: one new nullable column and a partial index; nothing existing is changed or dropped.
-- ADDITIVE: older binaries ignore it.
--
-- A fetch node's response is archived as new evidence (ADR-0114 D10). It must carry
-- its parent's classification and parties: the evidence ingestor copies them from the
-- row named here, never widening them (memo §12). No foreign key: an erasure deletes
-- evidence rows (ADR-0137), and a link must not block it.
--
-- IDEMPOTENT (IF NOT EXISTS). Rollback: DROP INDEX IF EXISTS idx_evidence_derived_from;
-- ALTER TABLE evidence DROP COLUMN IF EXISTS derived_from;

ALTER TABLE evidence ADD COLUMN IF NOT EXISTS derived_from TEXT;
CREATE INDEX IF NOT EXISTS idx_evidence_derived_from ON evidence (derived_from) WHERE derived_from IS NOT NULL;
