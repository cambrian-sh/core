-- 0011_evidence.sql — the evidence foundation (ADR-0105, Knowledge Substrate Phase 1).
--
-- Preserve what arrived, immutably, BEFORE any semantic processing. An evidence row is
-- written only after its original bytes are durable in the content-addressed store
-- (content-first commit ordering, ADR-0105 D2), so a row here always points at
-- reprocessable source material. Rows are never updated: a changed source artifact is a
-- NEW row with a higher source_revision linked by revises_id (D4).
--
-- ADDITIVE, per the standing rule (ADR-0093 D6): nothing renamed, nothing dropped, no
-- existing row rewritten. Rollback is `DROP TABLE evidence_outbox; DROP TABLE evidence;`.

CREATE TABLE IF NOT EXISTS evidence (
	id              TEXT PRIMARY KEY,
	-- Constant-valued today ('default'), carried on every foundational table and inside
	-- every unique index from day one because retrofitting it across evidence, items,
	-- resolutions and their indexes later is the expensive path (ADR-0105 D5).
	namespace_id    TEXT NOT NULL DEFAULT 'default',
	-- Source identity and the source-native key + revision of the artifact. The triple
	-- (source_id, source_key, source_revision) inside a namespace IS the idempotency key:
	-- a replay of the same delivery creates no new version (D3).
	source_id       TEXT NOT NULL,
	source_key      TEXT NOT NULL,
	source_revision TEXT NOT NULL DEFAULT '',
	-- Two clocks, deliberately separate (memo §8): source_time is what the sender's clock
	-- claimed — attacker-controllable, never used for latency; ingested_at is when WE
	-- received it — the "could we have known?" clock.
	source_time     TIMESTAMPTZ,
	ingested_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
	-- Pointer into the content-addressed store (domain.ContentStore, ADR-0022). The bytes
	-- are durable before this row exists; content_bytes is recorded so retention math never
	-- needs to fetch the blob.
	content_hash    TEXT NOT NULL,
	content_bytes   BIGINT NOT NULL DEFAULT 0,
	-- Source classification as delivered. Technical delivery fields (cursor, trace) stop
	-- HERE — the envelope terminates at Evidence; derived records link, never copy
	-- (memo §19.2).
	classification  TEXT[] NOT NULL DEFAULT '{}',
	cursor          TEXT NOT NULL DEFAULT '',
	trace_id        TEXT NOT NULL DEFAULT '',
	revises_id      TEXT REFERENCES evidence(id),
	CONSTRAINT evidence_source_revision_unique
		UNIQUE (namespace_id, source_id, source_key, source_revision)
);

CREATE INDEX IF NOT EXISTS idx_evidence_source ON evidence (namespace_id, source_id, source_key);
CREATE INDEX IF NOT EXISTS idx_evidence_ingested_at ON evidence (ingested_at);

-- The transactional outbox (memo §11): inserted in the SAME transaction as its evidence
-- row, consumed at-least-once by the (Phase 2) transformation worker. processed_at is the
-- exactly-once-logical transition: UPDATE … WHERE processed_at IS NULL, and the row count
-- is the truth — a second consumer observes zero rows.
CREATE TABLE IF NOT EXISTS evidence_outbox (
	id           BIGSERIAL PRIMARY KEY,
	namespace_id TEXT NOT NULL DEFAULT 'default',
	evidence_id  TEXT NOT NULL REFERENCES evidence(id),
	created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
	processed_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_evidence_outbox_pending
	ON evidence_outbox (created_at) WHERE processed_at IS NULL;
