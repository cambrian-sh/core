-- 0043_object_fence_classes.sql — interned fence classes for the object tables
-- (ADR-0149 D3/D6; T-20261113-unfiltered-listobjects-group-list-under-a;
-- cambrian-knowledge/investigations/2026-11-13-unfiltered-listobjects-existence-cost.md).
--
-- The access fence (internal/authz/sqlfence) decides a property value's
-- visibility from ONE (tags, parties) pair and only through set operations, so
-- every distinct canonical pair is judged once per request, not once per row
-- per property. This is the dictionary of those pairs. Each objects_<kind> row
-- carries vis_classes: the ids of its declared properties' pairs plus the pair of
-- the evidence that minted the entity. A ListObjects caller's visible set V is
-- computed once through the fence over this table, and an object exists for the
-- caller (ADR-0128 D12) exactly when vis_classes meets V.
--
-- Canonical form: tags and parties sorted and de-duplicated, parties '{}' when
-- absent. A NULL tag column (no classification) is never a class. Written only
-- by the object maintainer (internal/objects/state.go; arch_test.go), append-only:
-- a row is never updated or deleted, so an id names one pair forever.
--
-- ADDITIVE. Rollback: DROP TABLE object_fence_classes; (the objects_<kind>
-- tables' vis_classes column is then dropped by hand, and a rebuild refills it).

CREATE TABLE IF NOT EXISTS object_fence_classes (
	id      INTEGER GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	tags    TEXT[] NOT NULL,
	parties TEXT[] NOT NULL,
	CONSTRAINT object_fence_classes_pair_unique UNIQUE (tags, parties)
);
