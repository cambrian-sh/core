-- 0054_typed_row_classification.sql — row-level classification on typed rows
-- (ADR-0158 D1, D7; stage 1, ENG-1974 under ENG-1838).
-- ADDITIVE: four catalog-only columns; nothing existing is changed or rewritten.
-- cambrian:not-destructive: adds columns with a constant default, deletes and rewrites nothing.
--
--   events.classification, observations.classification, links.classification
--       the row's OWN classification: tags that can only narrow who sees the row,
--       on top of what its evidence row allows. The fence is
--       fence(E) AND fence(E ∪ R) (ADR-0158 D2), so an empty R reads exactly as
--       before. observations is partitioned: the column is added on the parent and
--       reaches every partition.
--   entities.first_seen_classification
--       the row classification of the row that minted the entity (ADR-0158 D4 b),
--       write-once like first_seen_evidence.
--
-- NO BACKFILL (D7): every existing row gets '{}', and fence(E) AND fence(E ∪ ∅) =
-- fence(E). No mapping can have written restricted rows yet (ENG-1041 refuses a
-- mapping classification until the last ADR-0158 stage). ADD COLUMN with a constant
-- default is a catalog-only change on Postgres 11+ (no table rewrite).
--
-- object_fence_classes.row_tags and the object-table <prop>__rowtags companions
-- (ADR-0158 D2/D7, objects) ship with their consumer in stage 4.
--
-- IDEMPOTENT (IF NOT EXISTS). Rollback:
--   ALTER TABLE events DROP COLUMN classification;
--   ALTER TABLE observations DROP COLUMN classification;
--   ALTER TABLE links DROP COLUMN classification;
--   ALTER TABLE entities DROP COLUMN first_seen_classification;

ALTER TABLE events       ADD COLUMN IF NOT EXISTS classification TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE observations ADD COLUMN IF NOT EXISTS classification TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE links        ADD COLUMN IF NOT EXISTS classification TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE entities     ADD COLUMN IF NOT EXISTS first_seen_classification TEXT[] NOT NULL DEFAULT '{}';
