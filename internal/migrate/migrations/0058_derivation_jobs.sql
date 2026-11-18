-- 0058_derivation_jobs.sql — the durable journal of post-ingest derivations (ADR-0136 D4
-- facts, ADR-0053 KG chunk triplets; ENG-2122).
-- cambrian:not-destructive: one new table; nothing existing is changed or dropped.
-- ADDITIVE: older binaries ignore it.
--
-- Fact and triplet extraction run from in-memory queues after the ingest is
-- acknowledged. A restart, a failed LLM call or a full queue lost the job for good:
-- the document's chunks were stored, its facts and triplets never appeared, and
-- nothing re-queued them. The ingest now records one row per derivation kind BEFORE
-- it saves the chunks; the row is deleted when the derivation finished (or was
-- refused for good by the erasure / source-version fence) and otherwise stays for the
-- kernel's recovery loop, which re-feeds the same extractor after a restart and after
-- a failure, with backoff.
--
--   kind             'facts' | 'kg_triplets'.
--   document_id      the stored document; (kind, document_id) is the natural key
--                    (AGENTS.md §2.6): a re-ingest of the document replaces its row.
--   content_digest   the version the job was queued for (ENG-501): a replay is fenced
--                    with it, and completion deletes only the row of that version.
--   chunk_ids        the document's stored chunks in ingest order (the fact content
--                    hash and turn indexes depend on it). Identifiers only — the text
--                    is read back through the enforcing read store.
--   payload          what the in-memory job carried on its context: writer principal,
--                    evidence id, ingest origin, A2.1 source metadata (title, author —
--                    which is why erasure deletes a document's rows).
--   attempts         failed extraction attempts; a queue-full deferral is not one.
--   claimed_at       when a kernel process last took the row (ingest or recovery); a
--                    row claimed before the running process started is orphaned work.
--   next_attempt_at  when the row is next due for the recovery loop (the claim lease,
--                    or the retry backoff after a failure).
--   dead_at          set after the last allowed attempt: never retried automatically,
--                    still counted in the backlog.
--
-- Rollback: DROP TABLE derivation_jobs;

CREATE TABLE IF NOT EXISTS derivation_jobs (
    kind            TEXT NOT NULL CHECK (kind IN ('facts', 'kg_triplets')),
    document_id     TEXT NOT NULL CHECK (document_id <> ''),
    content_digest  TEXT NOT NULL DEFAULT '',
    chunk_ids       TEXT[] NOT NULL,
    payload         JSONB NOT NULL DEFAULT '{}'::jsonb,
    attempts        INT NOT NULL DEFAULT 0,
    last_error      TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    claimed_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    dead_at         TIMESTAMPTZ,
    PRIMARY KEY (kind, document_id)
);
CREATE INDEX IF NOT EXISTS idx_derivation_jobs_due
    ON derivation_jobs (next_attempt_at)
    WHERE dead_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_derivation_jobs_claimed
    ON derivation_jobs (claimed_at)
    WHERE dead_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_derivation_jobs_document
    ON derivation_jobs (document_id);
