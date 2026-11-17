-- 0055_object_fence_class_row_tags.sql — fence classes become (tags, parties,
-- row_tags) triples (ADR-0158 D2/D7, objects; stage 4, ENG-1986 under ENG-1838).
-- cambrian:not-destructive: adds a column with a constant default and widens a unique key; no row is deleted or rewritten.
--
-- An object-table property value now carries the row classification of the
-- observation it came from (<prop>__rowtags, written by internal/objects), and
-- its fence is fence(tags) AND fence(tags || row_tags) over the same parties
-- (sqlfence ObjectProperty(...).WithRowTags). The dictionary of fence classes the
-- existence rule reads (0043) must therefore intern the triple, so that a class
-- id still names exactly one fence decision: row_tags defaults to '{}', and every
-- existing pair becomes the triple (tags, parties, '{}') WITH ITS ID UNCHANGED —
-- the vis_classes arrays already written stay valid, and for row_tags = '{}' the
-- triple's fence is the pair's fence (sqlfence.FenceClassRowTags).
--
-- The unique key moves from (tags, parties) to (tags, parties, row_tags). Building
-- it reads the dictionary once (one row per distinct classification combination;
-- small). The old key is dropped only after the new one exists, in this file's
-- transaction.
--
-- object_fence_classes_row_tagged is a partial index holding only triples with a
-- non-empty row classification: the objects reader asks "does any exist?" once per
-- filtered request (internal/objects visibleClasses), and renders the row-tags
-- fence only when one does. Empty until a mapping writes row classification.
--
-- IDEMPOTENT. Rollback (only while no triple with row_tags <> '{}' exists):
--   DROP INDEX IF EXISTS object_fence_classes_row_tagged;
--   ALTER TABLE object_fence_classes DROP CONSTRAINT IF EXISTS object_fence_classes_triple_unique;
--   ALTER TABLE object_fence_classes ADD CONSTRAINT object_fence_classes_pair_unique UNIQUE (tags, parties);
--   ALTER TABLE object_fence_classes DROP COLUMN row_tags;

ALTER TABLE object_fence_classes ADD COLUMN IF NOT EXISTS row_tags TEXT[] NOT NULL DEFAULT '{}';

DO $$
BEGIN
	IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'object_fence_classes_triple_unique') THEN
		ALTER TABLE object_fence_classes ADD CONSTRAINT object_fence_classes_triple_unique UNIQUE (tags, parties, row_tags);
	END IF;
END $$;

ALTER TABLE object_fence_classes DROP CONSTRAINT IF EXISTS object_fence_classes_pair_unique;

CREATE INDEX IF NOT EXISTS object_fence_classes_row_tagged ON object_fence_classes (id) WHERE cardinality(row_tags) > 0;
