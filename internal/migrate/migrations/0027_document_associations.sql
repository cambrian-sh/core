-- 0027_document_associations.sql — the sessions and classification scopes an operator
-- upload was delivered in (ADR-0138 Amendment B1.1).
-- ADDITIVE: one new table; nothing is changed or dropped, and older binaries ignore it.
--
-- An exact re-upload by the same writer (same revision digest) writes NO chunks: the
-- stored document is reused. This table is what records that it was uploaded again,
-- and where — the session and the classification scope of the delivery. It is
-- bookkeeping, content-free, and does NOT change who may read the document: the
-- document's classification stays its stored tags.
--
-- Key: the natural key (document, writer, session, scope), so a retry or a repeat
-- upload in the same place updates one row (deliveries + 1, last_seen_at) instead of
-- inserting a duplicate (AGENTS.md §2.6). scope is the normalised, sorted tag set
-- (internal/memory uploadScope); '' is "no session".
--
-- The (written_by, content_digest) lookup index on documents is built CONCURRENTLY by
-- the runner (migrate.go concurrentIndexes: idx_documents_writer_digest), not here.
--
-- Rollback: DROP TABLE document_associations;

CREATE TABLE IF NOT EXISTS document_associations (
	document_id  TEXT NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
	writer       TEXT NOT NULL,
	session_id   TEXT NOT NULL DEFAULT '',
	scope        TEXT[] NOT NULL DEFAULT '{}',
	upload_name  TEXT NOT NULL DEFAULT '',
	first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
	deliveries   INT NOT NULL DEFAULT 1,
	PRIMARY KEY (document_id, writer, session_id, scope)
);
