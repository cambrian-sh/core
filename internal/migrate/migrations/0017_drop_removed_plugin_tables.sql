-- 0017_drop_removed_plugin_tables.sql — remove the tables a deleted plugin left behind
-- (ADR-0064 runner; owner decision 2026-10-31,
-- T-20261031-owner-decision-add-a-migration-dropping-the).
--
-- WHY THIS LIVES IN CORE'S MIGRATION SET. The drift plugin (premium, PREM-0102) was
-- removed on 2026-10-31. It never had a migration: it ran CREATE TABLE IF NOT EXISTS at
-- plugin Build, so the seven tables below exist in every database a premium kernel ran
-- against, and nothing creates, reads or writes them any more. Premium has no migration
-- set of its own — every premium store creates its tables at runtime — so the only
-- versioned, recorded, run-once-per-database mechanism that reaches existing deployments
-- and dev databases is this runner, which the kernel applies on boot
-- (storage.auto_migrate) and operators apply with `orchestrator migrate up`. A
-- runtime DROP in a premium plugin would run on every boot forever and leave no record
-- that it had happened; that is the rejected alternative.
--
-- This file names table identifiers only. Core imports nothing from premium and gains no
-- knowledge of the removed product beyond "these names are garbage" (ADR-0057 boundary).
--
-- IDEMPOTENT. Every statement is IF EXISTS, so a database that never ran the plugin (the
-- OSS case, CI, a fresh install) is a no-op. Each table's indexes
-- (drift_records_current_uniq, drift_records_kind_key_idx,
-- drift_records_owner_current_idx, drift_records_superseded_idx,
-- drift_commands_claimed_idx, drift_refusals_at_idx, drift_alerts_raised_idx,
-- drift_alerts_cited_idx, drift_commitments_current_uniq,
-- drift_commitments_message_uniq) and BIGSERIAL sequences are owned by the table and go
-- with it; the plugin created no types, views or functions. No foreign key ever pointed
-- into these tables from a core table (drift_alerts.cited_versions is a BIGINT[] of
-- drift_records ids, not an FK), so no CASCADE is needed — and none is used, so that if
-- something unexpected DID depend on one of them the migration fails loudly instead of
-- silently dropping it.
--
-- DELIBERATELY NOT TOUCHED: knowledge_items / statement_values / resolutions rows of kind
-- 'commitment'. The plugin also wrote commitments into the core substrate (ADR-0106), but
-- the kind is DATA that other producers share (the substrate_retrieval benchmark and the
-- substrate ask-former use it), there is no producer marker that would separate the
-- plugin's rows from theirs, and 0012 commits the kernel to never branching on a specific
-- kind and never erasing interpretations. They stay.
--
-- Forward-only. Rollback is not meaningful: the code that would read these tables is gone.

DROP TABLE IF EXISTS drift_alerts;
DROP TABLE IF EXISTS drift_refusals;
DROP TABLE IF EXISTS drift_retention_runs;
DROP TABLE IF EXISTS drift_commitments;
DROP TABLE IF EXISTS drift_commands;
DROP TABLE IF EXISTS drift_record_pushes;
DROP TABLE IF EXISTS drift_records;
