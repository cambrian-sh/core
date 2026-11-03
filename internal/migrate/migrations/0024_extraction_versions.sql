-- 0024_extraction_versions.sql — versioned, coverage-carrying extraction markers
-- (ADR-0136 Amendment A2.4; fixes T-20261103-fact-extraction-markers-are-keyed-by-content).
-- ADDITIVE: three columns on knowledge_extractions (0022); nothing is changed or dropped.
--
-- Version number: 0023 is reserved for ADR-0137's erasure migration (0023_erasure.sql),
-- which is not built yet. The runner applies every embedded version a database has not
-- recorded, in version order (migrate.go), so a database that applies 0024 first still
-- applies 0023 when a binary carrying it boots.
--
-- 1. extractor_version — `<prompt id>/<generator model>` of the run that wrote the
--    marker (e.g. fact-extract-v2/claude-haiku-4-5). A marker of another version counts
--    as PENDING for facts-backfill and re-ingest, so a prompt or backbone change applies
--    to a stored corpus by backfill. Rows written before 0024 read '' (legacy): pending.
-- 2. passages_covered / passages_total — how many of the source's extractor passages
--    the run covered (A2.3 sectioned extraction under max_calls_per_document). A marker
--    with covered < total is PARTIAL; the backfill resumes it from the first uncovered
--    window. NULL on legacy rows (read as 0 of 0, and the '' version already makes
--    them pending).
--
-- The primary key stays (namespace_id, extractor, document_id, content_hash): a later run
-- over the same content REPLACES the row's version and coverage (MarkExtracted upserts).
--
-- Rollback: ALTER TABLE knowledge_extractions DROP COLUMN passages_total,
-- DROP COLUMN passages_covered, DROP COLUMN extractor_version.

ALTER TABLE knowledge_extractions ADD COLUMN IF NOT EXISTS extractor_version TEXT NOT NULL DEFAULT '';
ALTER TABLE knowledge_extractions ADD COLUMN IF NOT EXISTS passages_covered INT;
ALTER TABLE knowledge_extractions ADD COLUMN IF NOT EXISTS passages_total INT;
