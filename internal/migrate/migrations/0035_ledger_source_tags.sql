-- 0035_ledger_source_tags.sql — derived ledger entries inherit the classification
-- of what they were built from (ADR-0145 D1; T-20261108-ledger-step-outputs-do-not-inherit-the).
-- ADDITIVE: one new column with a default; older binaries ignore it.
--
--   source_tags  JSON array of tag arrays: the classifications of every entry a
--                step output or calculation was derived from (its step's tool
--                results and its upstream inputs, transitively). The read gate
--                (authz.EnforcingLedger) requires the reader's predicate to admit
--                tags AND every array here.
--
-- Rollback: ALTER TABLE ledger_entries DROP COLUMN source_tags;

ALTER TABLE ledger_entries ADD COLUMN IF NOT EXISTS source_tags JSONB NOT NULL DEFAULT '[]'::jsonb;
