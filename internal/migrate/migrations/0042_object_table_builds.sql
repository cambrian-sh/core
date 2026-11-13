-- 0042_object_table_builds.sql — bookkeeping for the generated object tables
-- (ADR-0149 D3, P3; T-20261113-adr-0149-p3-per-kind-object-tables-generated).
--
-- The objects_<kind> tables themselves are NOT created here: internal/objects
-- generates them from the live ontology declarations (boot and every applied
-- change), because a kind is data, not code. This table records, per generated
-- table, the fingerprint of the declarations its rows were last rebuilt from
-- (objects.Table.Hash). A table whose fingerprint differs from the current
-- declarations — a new table kind, a new property — is rebuilt from history.
--
-- ADDITIVE. Rollback: DROP TABLE object_table_builds; (and DROP TABLE each
-- objects_<kind> table, which history rebuilds on the next boot).

CREATE TABLE IF NOT EXISTS object_table_builds (
	table_name TEXT PRIMARY KEY,
	spec_hash  TEXT NOT NULL,
	built_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
