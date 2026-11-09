-- 0036_ledger_composition_kind.sql — the run ledger records worker compositions
-- (ADR-0145 D12; T-20261108-adr-0145-d12-kernel-composes-plan-step-workers).
-- Widens the kind CHECK by one value; existing rows are unaffected, and an older
-- binary never writes the new kind.
--
--   composition  the worker the kernel composed for a plan step: runtime, the
--                professions, the skill index, the tools, the step's effects, the
--                brief and the input ledger references (domain.WorkerComposition).
--
-- The constraint is the one 0034 declared inline (Postgres names it
-- ledger_entries_kind_check).
--
-- Rollback: DELETE FROM ledger_entries WHERE kind = 'composition'; then re-add the
-- 0034 constraint without 'composition'.

ALTER TABLE ledger_entries DROP CONSTRAINT IF EXISTS ledger_entries_kind_check;
ALTER TABLE ledger_entries ADD CONSTRAINT ledger_entries_kind_check
    CHECK (kind IN ('tool_result','step_output','computed','artifact','scratch','composition'));
