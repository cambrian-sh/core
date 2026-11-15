-- 0051_memory_erasure.sql — erasure state, derivative foreign keys and evidence FK
-- actions for the principal-initiated forget (ADR-0137 S1; ENG-411 stage A).
--
-- NUMBER: ADR-0137 reserved 0023 for this file. It ships as 0051 instead: core is at 0050,
-- and an out-of-order 0023 would run AFTER 0024–0050 on every existing database but at
-- position 23 on a fresh one — two histories for one file, over tables reshaped in between
-- (evidence_outbox 0039, entities 0048). Every migration from 0051 on is classified by its
-- marker (internal/migrate/destructive.go).
--
-- WHAT IT DOES
--   1. memory_erasures — one row per erase request: the OSS receipt of record (ADR-0137 D1,
--      D8). request_id is deterministic (domain.ErasureRequest.ID: selector + principal,
--      never a clock, D6), so a retry finds this row and returns the same receipt.
--   2. memory_erasure_targets — the content-free tombstone: one row per erased unit (ids,
--      the sha256 content digest of a document, erased_at = the erase transaction's time).
--      It is the future no-resurrection fence (D5, job B), hence the two lookup indexes.
--   3. memory_erasure_outbox — the async tail (D7): steps that cannot run inside the
--      Postgres transaction (CAS delete by CID, …), drained exactly once by the erasure
--      worker (job B; nothing drains it yet).
--   4. chunk_triplets.chunk_id and chunk_pagerank.chunk_id gain REFERENCES chunks(id)
--      ON DELETE CASCADE (D5.1; T-20261103-deleting-a-chunk-leaves-its-chunk-triplets-and):
--      a deleted chunk takes its KG triplets and PageRank row with it, and an in-flight
--      triplet write for a chunk that is gone fails instead of resurrecting it (the batcher
--      already drops a failed save, chunk_triplets_batcher.go). memory_facts no longer
--      exists (0021); fact items are erased through EraseItemsTx, not a chunk FK.
--   5. Evidence FK actions (D4.8): evidence_outbox rows are plumbing and cascade with their
--      evidence; entities.first_seen_evidence and evidence.revises_id are id handles, not
--      content, and become NULL. knowledge_items, events and links keep NO ACTION on
--      purpose: they are content derived from the evidence, the erase transaction deletes
--      and counts them explicitly, and NO ACTION turns a forgotten dependent into an error
--      instead of a silent, uncounted cascade from some other delete path.
--
-- DESTRUCTIVE (marker below): step 4 first deletes ORPHAN rows — chunk_triplets and
-- chunk_pagerank rows whose chunk no longer exists (the documents cascade and loner decay
-- left them behind). Only rows no read can reach through their chunk are removed; no chunk,
-- document or evidence row is touched (ADR-0095 D8: never destroy the corpus to tidy a
-- schema). The runner snapshots a database with history before applying it.
-- cambrian:destructive orphan sweep of chunk_triplets / chunk_pagerank before their FKs
--
-- IDEMPOTENT: CREATE … IF NOT EXISTS; every constraint is dropped IF EXISTS and re-added.
-- Rollback: DROP TABLE memory_erasure_outbox, memory_erasure_targets, memory_erasures;
--   ALTER TABLE chunk_triplets DROP CONSTRAINT chunk_triplets_chunk_id_fkey;
--   ALTER TABLE chunk_pagerank DROP CONSTRAINT chunk_pagerank_chunk_id_fkey; and re-add the
--   three evidence FKs without an ON DELETE action. The orphan rows are not restorable
--   (they were unreachable).

CREATE TABLE IF NOT EXISTS memory_erasures (
	request_id      TEXT PRIMARY KEY,
	-- Who asked: an agent principal, an operator, or the kernel. Ids only.
	principal       TEXT NOT NULL,
	principal_kind  TEXT NOT NULL,
	-- A closed enum (domain.ErasureReason*), never free text.
	reason_code     TEXT NOT NULL,
	-- Which selector kinds the request named (document, thread, chunk, experience, entity).
	selector_kinds  TEXT[] NOT NULL DEFAULT '{}',
	-- complete | pending_async (outbox steps remain).
	status          TEXT NOT NULL,
	erased_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
	-- Set by the erasure worker's configuration (job B); NULL until then.
	async_deadline  TIMESTAMPTZ,
	completed_at    TIMESTAMPTZ,
	-- Rows deleted per store, {table: n}. Counts only.
	counts          JSONB NOT NULL DEFAULT '{}'
);

CREATE INDEX IF NOT EXISTS idx_memory_erasures_principal ON memory_erasures (principal, erased_at DESC);

CREATE TABLE IF NOT EXISTS memory_erasure_targets (
	request_id     TEXT NOT NULL REFERENCES memory_erasures(request_id) ON DELETE CASCADE,
	-- document | chunk | experience | entity | evidence | thread
	target_kind    TEXT NOT NULL,
	target_id      TEXT NOT NULL,
	-- The document the unit belonged to, when it had one.
	document_id    TEXT,
	-- sha256 of the erased document body (documents.content_digest), never the body.
	content_digest TEXT,
	erased_at      TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (request_id, target_kind, target_id)
);

CREATE INDEX IF NOT EXISTS idx_memory_erasure_targets_document
	ON memory_erasure_targets (document_id, erased_at) WHERE document_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_memory_erasure_targets_digest
	ON memory_erasure_targets (content_digest, erased_at) WHERE content_digest IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_memory_erasure_targets_target
	ON memory_erasure_targets (target_kind, target_id);

CREATE TABLE IF NOT EXISTS memory_erasure_outbox (
	id            BIGSERIAL PRIMARY KEY,
	request_id    TEXT NOT NULL REFERENCES memory_erasures(request_id) ON DELETE CASCADE,
	-- The async step (domain.ErasureStep*), e.g. cas_delete.
	step          TEXT NOT NULL,
	-- A content-free reference the step acts on (a CID, an id).
	ref           TEXT NOT NULL,
	created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
	processed_at  TIMESTAMPTZ,
	attempts      INT NOT NULL DEFAULT 0,
	-- Error class only; a worker must never write content here.
	last_error    TEXT NOT NULL DEFAULT '',
	CONSTRAINT memory_erasure_outbox_step_unique UNIQUE (request_id, step, ref)
);

CREATE INDEX IF NOT EXISTS idx_memory_erasure_outbox_pending
	ON memory_erasure_outbox (created_at, id) WHERE processed_at IS NULL;

-- 4. Orphan sweep, then the derivative FKs.
DELETE FROM chunk_triplets t
 WHERE NOT EXISTS (SELECT 1 FROM chunks c WHERE c.id = t.chunk_id);
DELETE FROM chunk_pagerank p
 WHERE NOT EXISTS (SELECT 1 FROM chunks c WHERE c.id = p.chunk_id);

ALTER TABLE chunk_triplets DROP CONSTRAINT IF EXISTS chunk_triplets_chunk_id_fkey;
ALTER TABLE chunk_triplets ADD CONSTRAINT chunk_triplets_chunk_id_fkey
	FOREIGN KEY (chunk_id) REFERENCES chunks(id) ON DELETE CASCADE;
ALTER TABLE chunk_pagerank DROP CONSTRAINT IF EXISTS chunk_pagerank_chunk_id_fkey;
ALTER TABLE chunk_pagerank ADD CONSTRAINT chunk_pagerank_chunk_id_fkey
	FOREIGN KEY (chunk_id) REFERENCES chunks(id) ON DELETE CASCADE;

-- 5. Evidence FK actions.
ALTER TABLE evidence_outbox DROP CONSTRAINT IF EXISTS evidence_outbox_evidence_id_fkey;
ALTER TABLE evidence_outbox ADD CONSTRAINT evidence_outbox_evidence_id_fkey
	FOREIGN KEY (evidence_id) REFERENCES evidence(id) ON DELETE CASCADE;
ALTER TABLE evidence DROP CONSTRAINT IF EXISTS evidence_revises_id_fkey;
ALTER TABLE evidence ADD CONSTRAINT evidence_revises_id_fkey
	FOREIGN KEY (revises_id) REFERENCES evidence(id) ON DELETE SET NULL;
ALTER TABLE entities DROP CONSTRAINT IF EXISTS entities_first_seen_evidence_fkey;
ALTER TABLE entities ADD CONSTRAINT entities_first_seen_evidence_fkey
	FOREIGN KEY (first_seen_evidence) REFERENCES evidence(id) ON DELETE SET NULL;
