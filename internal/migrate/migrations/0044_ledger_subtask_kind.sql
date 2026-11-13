-- 0044_ledger_subtask_kind.sql — the run ledger records kernel-run subtasks
-- (ADR-0150 D2; T-20261113-adr-0150-slice-2-run-subtasks-kernel-tool-brief).
-- Widens the kind CHECK by one value; existing rows are unaffected, and an older
-- binary never writes the new kind.
--
--   subtask  one subtask the single-agent lane's agent ran with run_subtasks:
--            its deterministic id, the brief, the worker's tools and effect
--            ceiling, the status and the ledger ref of the full answer
--            (internal/subtasks).
--
-- Rollback: DELETE FROM ledger_entries WHERE kind = 'subtask'; then re-add the
-- 0036 constraint without 'subtask'.

ALTER TABLE ledger_entries DROP CONSTRAINT IF EXISTS ledger_entries_kind_check;
ALTER TABLE ledger_entries ADD CONSTRAINT ledger_entries_kind_check
    CHECK (kind IN ('tool_result','step_output','computed','artifact','scratch','composition','subtask'));
