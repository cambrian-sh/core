-- 0062_derivation_jobs_background_embed.sql — background embedding of large ingests as a
-- third derivation kind of the ENG-2122 journal (T-20261118-large-sync-ingest-blocks-190-s-on-embedding-and,
-- ENG-2143; builds on 0058).
-- cambrian:not-destructive: widens the kind CHECK by one value and adds two columns (one nullable, one defaulted); no row is deleted or rewritten.
-- ADDITIVE: older binaries claim only 'facts' and 'kg_triplets' rows by kind and never read the new columns.
--
-- A 5 MB text (5295 chunks) held a synchronous IngestMemory for ~190 s, all of it
-- embedding and saving the chunks. A document with at least
-- execution.ingestion.background_embed_min_chunks chunks is now ACCEPTED at once: its
-- evidence, document row and entity are written, its prepared chunks are staged in an
-- 'embed' row of this journal, and the call returns. The journaler embeds and saves the
-- chunks slice by slice in the background, shrinking chunk_ids to the chunks still to
-- embed after every slice, so a restart resumes where the previous process stopped.
--
--   kind           += 'embed'.
--   staged         the accepted document's prepared chunks (text, metadata, event
--                  stamps) and its structure-graph rows — the only state the background
--                  work needs. Set on 'embed' rows only; NULL on the others. It holds
--                  document content, which is why erasure deletes a document's rows
--                  (as it already did for the payload's title and author).
--   chunks_total   the chunk count of the accepted version, so progress reads as
--                  chunks_total - cardinality(chunk_ids) embedded of chunks_total.
--
-- Rollback: DELETE FROM derivation_jobs WHERE kind = 'embed'; ALTER TABLE derivation_jobs
-- DROP COLUMN staged, DROP COLUMN chunks_total; and restore the two-value CHECK.

ALTER TABLE derivation_jobs DROP CONSTRAINT IF EXISTS derivation_jobs_kind_check;
ALTER TABLE derivation_jobs ADD CONSTRAINT derivation_jobs_kind_check
    CHECK (kind IN ('facts', 'kg_triplets', 'embed'));
ALTER TABLE derivation_jobs ADD COLUMN IF NOT EXISTS staged JSONB;
ALTER TABLE derivation_jobs ADD COLUMN IF NOT EXISTS chunks_total INT NOT NULL DEFAULT 0;
