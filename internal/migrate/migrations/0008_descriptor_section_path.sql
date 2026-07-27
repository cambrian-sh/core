-- 0008_descriptor_section_path.sql — finish what 0007 claimed (ADR-0093).
--
-- 0007 gave `tools`, `skills` and `agent_profiles` "the same column set as chunks, minus
-- document parentage", so that the adapter could keep ONE row shape and one SELECT list.
-- It did not actually check that claim against the SELECT list, and `section_path` was
-- missing from all three.
--
-- The consequence only appeared on a live boot: `GetByID` consults every table that can
-- hold a retrievable document, so reading an agent profile emitted
--
--   ERROR: column "section_path" does not exist (SQLSTATE 42703)
--
-- and every agent's interview-vector check failed. No unit or integration test caught it,
-- because none of them read a descriptor back through the shared SELECT against the real
-- schema. That is the honest lesson: the row shape is a CONTRACT between the migration and
-- the adapter's column list, and nothing yet asserts the two agree.
--
-- Fixed forward rather than by editing 0007, which is already applied (ADR-0064:
-- append-only). Additive and idempotent; the default matches what chunks use.

ALTER TABLE tools          ADD COLUMN IF NOT EXISTS section_path TEXT NOT NULL DEFAULT '';
ALTER TABLE skills         ADD COLUMN IF NOT EXISTS section_path TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_profiles ADD COLUMN IF NOT EXISTS section_path TEXT NOT NULL DEFAULT '';
