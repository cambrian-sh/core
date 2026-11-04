-- 0025_extraction_windows.sql — the two-phase fact backfill's extraction journal
-- (ADR-0136 A2 "Backfill concurrency"; T-20261103-fact-backfill-processes-each-principal-s).
-- ADDITIVE: one new table; nothing is changed or dropped.
--
-- With execution.facts.backfill_group_concurrency > 1 the backfill extracts and stores
-- every window of a classification group BEFORE it assesses any of them (the assessment
-- pass then runs in event-time order against the stored corpus). The extraction marker
-- (knowledge_extractions) means "extracted AND assessed", so it cannot advance in
-- between; this journal records, per window, what extraction stored — the passage range
-- and the items as stored, with the subset the window inserted — so a run that stops
-- between the phases resumes with NO second extraction call and still assesses every
-- window. Ids only, no content. A document's rows are deleted when it commits, and
-- cmd/facts-reset deletes them with the markers.
--
-- Rollback: DROP TABLE knowledge_extraction_windows.

CREATE TABLE IF NOT EXISTS knowledge_extraction_windows (
	namespace_id      TEXT NOT NULL DEFAULT 'default',
	extractor         TEXT NOT NULL,
	document_id       TEXT NOT NULL,
	content_hash      TEXT NOT NULL,
	extractor_version TEXT NOT NULL,
	passage_start     INT  NOT NULL,
	passage_end       INT  NOT NULL,
	item_ids          TEXT[] NOT NULL DEFAULT '{}',
	new_item_ids      TEXT[] NOT NULL DEFAULT '{}',
	created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (namespace_id, extractor, document_id, content_hash, extractor_version, passage_start)
);
CREATE INDEX IF NOT EXISTS idx_knowledge_extraction_windows_document
	ON knowledge_extraction_windows (extractor, document_id);
