-- ENG-1043 item 1 (owner decision 2026-11-14): an entity handle is identified by
-- (namespace_id, id), not by id alone. 0016 declared `id TEXT PRIMARY KEY` while
-- every read filters by namespace_id, so minting the same scoped id in a second
-- namespace was a silent ON CONFLICT no-op (latent while every row is
-- 'default', ADR-0105 D5). PgEntityStore.EnsureEntity now conflicts on
-- (namespace_id, id).
--
-- Existing rows keep working: ids were unique on their own, so they are unique
-- per namespace too and the new key builds without touching a row. No table
-- references entities(id) by foreign key. The plain index on id keeps the
-- handful of id-only lookups (identity scans, lineage joins) on an index.
--
-- Rollback: ALTER TABLE entities DROP CONSTRAINT entities_pkey;
--           ALTER TABLE entities ADD CONSTRAINT entities_pkey PRIMARY KEY (id);
--           DROP INDEX IF EXISTS idx_entities_id;
-- (only while no id is minted in two namespaces).

ALTER TABLE entities DROP CONSTRAINT IF EXISTS entities_pkey;
ALTER TABLE entities ADD CONSTRAINT entities_pkey PRIMARY KEY (namespace_id, id);
CREATE INDEX IF NOT EXISTS idx_entities_id ON entities (id);
