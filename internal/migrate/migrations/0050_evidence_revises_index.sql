-- 0050_evidence_revises_index.sql — index evidence.revises_id (owner decision
-- 2026-11-14, ENG-1026).
--
-- Event lists and counts now keep only the latest revision of an edited occurrence:
-- the event whose evidence no later evidence row revises
-- (postgres.latestRevision: NOT EXISTS (SELECT 1 FROM evidence WHERE revises_id = ...)).
-- Without this index that probe is a scan of evidence per event row. Partial: only
-- revisions carry revises_id.
--
-- IDEMPOTENT (IF NOT EXISTS). Rollback: DROP INDEX IF EXISTS idx_evidence_revises_id;

CREATE INDEX IF NOT EXISTS idx_evidence_revises_id ON evidence (revises_id) WHERE revises_id IS NOT NULL;
