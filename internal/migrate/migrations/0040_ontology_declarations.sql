-- 0040_ontology_declarations.sql — live ontology declarations (ADR-0149 D2, P1;
-- T-20261113-adr-0149-p1-live-ontology-declarations).
--
-- The kind and relation registries were built once at boot. This table makes
-- them versioned data: every row declares kind and relation specs, and a
-- namespace's declarations at version v are the base rows (namespace '*', what
-- code and deployment config declare at boot) overlaid by that namespace's own
-- rows, each spec replacing the earlier spec of the same name.
--
--   version       one sequence for the whole table, so (namespace, version)
--                 names exactly one set of declarations and the order of a base
--                 change and a namespace change is never ambiguous.
--   namespace     '*' for the boot base; otherwise the namespace the change was
--                 applied to.
--   declarations  {"kinds":[KindSpec…], "relations":[RelationSpec…]} — the specs
--                 this version declares (domain.OntologyDoc).
--   author        "boot" for a base row; the operator who applied a change.
--
-- Append-only (the skill_ledger precedent): a version that could be rewritten
-- would make every decl_version recorded on a row point at something else.
--
-- decl_version on knowledge_items, events, observations and links records the
-- declarations version a write was checked against. NULL = written before this
-- migration, or by a tool holding a fixed registry.
--
-- ADDITIVE. Rollback: ALTER TABLE links DROP COLUMN decl_version; ALTER TABLE
--   observations DROP COLUMN decl_version; ALTER TABLE events DROP COLUMN
--   decl_version; ALTER TABLE knowledge_items DROP COLUMN decl_version;
--   DROP TABLE ontology_declarations; DROP FUNCTION ontology_declarations_append_only();

CREATE TABLE IF NOT EXISTS ontology_declarations (
	version      BIGINT PRIMARY KEY,
	namespace    TEXT NOT NULL,
	declarations JSONB NOT NULL,
	author       TEXT NOT NULL,
	created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
	CONSTRAINT ontology_declarations_version_positive CHECK (version > 0)
);

CREATE INDEX IF NOT EXISTS idx_ontology_declarations_namespace
	ON ontology_declarations (namespace, version);

CREATE OR REPLACE FUNCTION ontology_declarations_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'ontology_declarations is append-only (ADR-0149 D2)';
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_ontology_declarations_append_only ON ontology_declarations;
CREATE TRIGGER trg_ontology_declarations_append_only BEFORE UPDATE OR DELETE ON ontology_declarations
    FOR EACH ROW EXECUTE FUNCTION ontology_declarations_append_only();

ALTER TABLE knowledge_items ADD COLUMN IF NOT EXISTS decl_version BIGINT;
ALTER TABLE events ADD COLUMN IF NOT EXISTS decl_version BIGINT;
ALTER TABLE observations ADD COLUMN IF NOT EXISTS decl_version BIGINT;
ALTER TABLE links ADD COLUMN IF NOT EXISTS decl_version BIGINT;
