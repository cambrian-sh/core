-- 0037_value_identity.sql — value identity for structured ingest (ADR-0147).
--
-- Two additive changes to the typed rows (events, observations):
--
-- 1. time_basis (ADR-0147 D8). Which clock occurred_at was taken from:
--      'event'     the source's own event/change time, declared by the mapping;
--      'received'  our receipt time, because the mapping declared none (live
--                  deliveries only; a history fill without event time is refused);
--      ''          written before this migration — unknown, never guessed.
--    A reader (and the future current-values rule) can then tell a source-dated
--    value from one we dated ourselves.
--
-- 2. typed_row_retirements (ADR-0147 D6, D11). A typed row stops counting when a
--    mapping repair supersedes it or its source record was deleted. Retirement is a
--    RECORD beside the row, never a delete and never an edit of the row: the row
--    keeps answering "what did we once believe", and the substrate stays
--    append-only. Keyed by the row's source_ref, which is unique per namespace for
--    events and per (namespace, occurred_at) for observations — retiring a ref
--    retires every row written under it, which is the unit a projection writes.
--    reinstated_at reopens a ref when a repair is rolled back to an older mapping
--    revision; re-retiring clears it again.
--
-- Reads that serve "current"/counted answers exclude refs with a live retirement
-- (reinstated_at IS NULL).
--
-- ADDITIVE. Rollback: DROP TABLE typed_row_retirements;
--   ALTER TABLE observations DROP COLUMN time_basis; ALTER TABLE events DROP COLUMN time_basis;

ALTER TABLE events ADD COLUMN IF NOT EXISTS time_basis TEXT NOT NULL DEFAULT '';
ALTER TABLE observations ADD COLUMN IF NOT EXISTS time_basis TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS typed_row_retirements (
	namespace_id  TEXT        NOT NULL DEFAULT 'default',
	source_ref    TEXT        NOT NULL CHECK (source_ref <> ''),
	-- Why the row stopped counting: 'superseded' (a mapping repair) or
	-- 'source_deleted' (a tombstone). DATA, checked by the domain, not here.
	reason        TEXT        NOT NULL,
	-- What retired it: "mapping_revision:<n>" or the tombstone evidence id.
	retired_by    TEXT        NOT NULL DEFAULT '',
	retired_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
	reinstated_at TIMESTAMPTZ,
	PRIMARY KEY (namespace_id, source_ref)
);
