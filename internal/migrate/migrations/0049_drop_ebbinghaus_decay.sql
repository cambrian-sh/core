-- 0049_drop_ebbinghaus_decay.sql — remove the Ebbinghaus decay function (owner decision
-- 2026-11-14, ENG-74, option c: decay is not wanted).
--
-- apply_ebbinghaus_decay(INT) was defined by 0001_baseline.sql and re-pointed at the
-- `chunks` table by 0007_split_documents.sql (ADR-0015 D4). Nothing ever scheduled it:
-- no migration calls cron.schedule, and its startup guard (CountStaleDocuments) was
-- deleted in the same change as this file. The function is dropped so the database no
-- longer advertises a mechanism the kernel does not run.
--
-- NOT touched: update_activation_strength (still called by
-- PgVectorAdapter.UpdateActivationStrength) and the activation_strength column, which
-- the nightly loner GC (internal/memory/worker.go) still reads.
--
-- IDEMPOTENT: DROP ... IF EXISTS; a fresh database and a replay are no-ops.
-- Rollback: re-run the CREATE OR REPLACE FUNCTION body from 0007_split_documents.sql.

DROP FUNCTION IF EXISTS apply_ebbinghaus_decay(INT);
