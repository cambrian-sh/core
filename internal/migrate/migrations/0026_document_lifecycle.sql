-- 0026_document_lifecycle.sql — writer of record, content digest and a content-free
-- version chain for ingested documents (ADR-0138 D3).
-- ADDITIVE: three columns on documents, one new table, one index; nothing is changed or
-- dropped, and older binaries ignore the new columns.
--
-- Version number: 0023 stays reserved for ADR-0137's erasure migration (tombstones,
-- outbox, FKs on chunk_triplets/chunk_pagerank). The runner applies every embedded
-- version a database has not recorded, in version order (migrate.go).
--
-- 1. documents.written_by — the kernel-stamped writer of record (ExternalDocument.Author).
--    NULL on rows written before 0026; the lifecycle then reads the source_agent_id its
--    chunks carry. A re-ingest of the same id by a DIFFERENT writer is refused
--    (ADR-0138 Q1) instead of taking the rows over.
-- 2. documents.content_digest — the ADR-0138 D1.3 revision digest of the stored version.
--    A re-ingest with a different digest retires the stored version (its chunks, KG
--    triplets, PageRank, structure, fact items, assessments and markers) before it
--    writes. NULL on legacy rows (compared by text instead).
-- 3. documents.version — 1-based; +1 per retirement.
-- 4. document_versions — one CONTENT-FREE row per retired version: ids, digest, writer,
--    times and per-table counts of what the retirement deleted. Never text, never an
--    embedding, never a fact value.
-- 5. idx_knowledge_extractions_document — the cascade deletes markers by document id
--    across namespaces and extractors; the primary key leads with namespace_id.
--
-- Rollback: DROP TABLE document_versions; DROP INDEX idx_knowledge_extractions_document;
-- ALTER TABLE documents DROP COLUMN version, DROP COLUMN content_digest,
-- DROP COLUMN written_by.

ALTER TABLE documents ADD COLUMN IF NOT EXISTS written_by TEXT;
ALTER TABLE documents ADD COLUMN IF NOT EXISTS content_digest TEXT;
ALTER TABLE documents ADD COLUMN IF NOT EXISTS version INT NOT NULL DEFAULT 1;

CREATE TABLE IF NOT EXISTS document_versions (
	document_id    TEXT NOT NULL,
	version        INT  NOT NULL,
	content_digest TEXT NOT NULL DEFAULT '',
	written_by     TEXT NOT NULL DEFAULT '',
	created_at     TIMESTAMPTZ,
	retired_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
	counts         JSONB NOT NULL DEFAULT '{}',
	PRIMARY KEY (document_id, version)
);

CREATE INDEX IF NOT EXISTS idx_knowledge_extractions_document
	ON knowledge_extractions (document_id);
