-- 0056_links_supporting_evidence.sql — every evidence row a link was derived from
-- (ENG-2032 with ENG-2030; ADR-0137 erasure cascade, ADR-0128 D5).
-- ADDITIVE: one catalog-only column and one partial index; nothing existing is changed.
-- cambrian:not-destructive: adds a column with a constant default and an index, deletes and rewrites nothing.
--
--   links.supporting_evidence
--       the evidence rows an assertion was DERIVED from besides its cited evidence_id
--       (an identity producer's proposal reads several rows: the matched alias's
--       observation, both handles' mints and routes, the earlier event). The erasure
--       cascade deletes a link when ANY of them is erased, so a link derived from
--       forgotten content does not outlive it. Not part of links_dedup: a producer
--       already folds the supporting set into its source_ref.
--
--   idx_links_supporting_evidence
--       GIN over the array for the cascade's `supporting_evidence && $1`. Partial:
--       only rows that carry a supporting set (producer rows and their
--       confirmations); every other row keeps '{}' and is not indexed.
--
-- NO BACKFILL: links written before this migration keep '{}' and cascade by
-- evidence_id exactly as before; the next producer pass writes rows that record
-- their supports. ADD COLUMN with a constant default is catalog-only (Postgres 11+).
--
-- IDEMPOTENT (IF NOT EXISTS). Rollback:
--   DROP INDEX IF EXISTS idx_links_supporting_evidence;
--   ALTER TABLE links DROP COLUMN supporting_evidence;

ALTER TABLE links ADD COLUMN IF NOT EXISTS supporting_evidence TEXT[] NOT NULL DEFAULT '{}';

CREATE INDEX IF NOT EXISTS idx_links_supporting_evidence
    ON links USING GIN (supporting_evidence)
    WHERE supporting_evidence <> '{}';
