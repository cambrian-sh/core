-- 0021_drop_memory_facts.sql — retire the memory_facts table (ADR-0136 Amendment A1).
--
-- The ADR-0136 fact layer moved onto the knowledge substrate (owner decision
-- 2026-11-03, A1): a fact is now a `memory_fact` knowledge item (0012), its
-- supersession chain is DERIVED by the `valid_time_supersession` resolution authority,
-- and its extraction marker is `knowledge_extractions` (0022). The two tables 0020
-- created have no reader or writer left in the kernel.
--
-- Why a follow-up DROP instead of rewriting 0020 in place (A1-Q7): the runner silently
-- skips a rewritten version that a database already applied (migrate.go, `applied[v]`),
-- and 0020 was most likely applied to cambrian_bench by the ADR-0136 gate kernel's boot.
-- A rewrite would leave that database with memory_facts and WITHOUT the new tables; a
-- deleted 0020 would brick it ("database is ahead of this binary"). Keeping 0020 and
-- dropping here is correct on every database: one that applied 0020 loses two unread
-- tables; a fresh one creates and drops them in consecutive migrations.
--
-- Data: memory_facts rows were never measured (no ADR-0136 fact arm ran). Nothing is
-- migrated into the substrate; a deployment that wants facts for stored documents runs
-- `go run ./cmd/facts-backfill` (A1.7), which re-extracts through the same extractor.
--
-- btree_gist (installed by 0020) is left installed: dropping an extension another
-- object might use is not this file's decision, and it is inert.
--
-- Rollback: none needed for the data (it was unread); re-creating the tables means
-- re-applying 0020's DDL by hand.

DROP TABLE IF EXISTS memory_fact_extractions;
DROP TABLE IF EXISTS memory_facts;
