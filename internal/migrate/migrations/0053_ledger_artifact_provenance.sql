-- 0053_ledger_artifact_provenance.sql — the run ledger becomes the one artifact
-- registry (ADR-0157 D5; ENG-500 S4).
-- ADDITIVE: four new columns with defaults and two indexes; older binaries ignore them.
-- cambrian:not-destructive: adds columns and indexes, deletes and rewrites nothing.
--
--   session_id    the task session of the lease the entry was written under, so
--                 ListStepArtifacts can address artifacts by (session, step).
--   instance_id   the agent instance that wrote the entry (the verified per-instance
--                 token, or the instance whose scratch out/ the kernel collected).
--   derived_from  JSON array of the ledger ids the entry was built from (its record's
--                 inputs plus the step's / turn's tool results it inherited).
--   source_refs   JSON array of the memory records (chunk, document, experience ids)
--                 the content quotes, transitively; the ADR-0137 erase deletes every
--                 entry whose source_refs meet what it erases (GIN index, ?| operator).
--
-- Rollback: ALTER TABLE ledger_entries DROP COLUMN source_refs, DROP COLUMN derived_from,
--           DROP COLUMN instance_id, DROP COLUMN session_id;

ALTER TABLE ledger_entries ADD COLUMN IF NOT EXISTS session_id   TEXT  NOT NULL DEFAULT '';
ALTER TABLE ledger_entries ADD COLUMN IF NOT EXISTS instance_id  TEXT  NOT NULL DEFAULT '';
ALTER TABLE ledger_entries ADD COLUMN IF NOT EXISTS derived_from JSONB NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE ledger_entries ADD COLUMN IF NOT EXISTS source_refs  JSONB NOT NULL DEFAULT '[]'::jsonb;
CREATE INDEX IF NOT EXISTS idx_ledger_entries_session ON ledger_entries (session_id, step_index) WHERE session_id <> '';
CREATE INDEX IF NOT EXISTS idx_ledger_entries_source_refs ON ledger_entries USING GIN (source_refs);
