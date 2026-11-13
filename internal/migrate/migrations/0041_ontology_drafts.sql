-- 0041_ontology_drafts.sql — agent-drafted ontology changes awaiting a person
-- (ADR-0149 D2: "Agents may draft changes; a person approves";
-- T-20261113-agent-drafted-ontology-changes-have-no-approval, owner option (a)).
--
-- An approval's arguments are not persisted, so a draft needs its own home that
-- survives a restart: the ontology_change approval names the draft in its Object
-- and its handler reads the change from here.
--
-- ontology_drafts — one row per draft, written once:
--   draft_id      deterministic from (namespace, canonical change, proposer,
--                 client ref) — a retried draft is the same row (AGENTS.md §2.6).
--   namespace     the namespace the change is for.
--   change        the proposed declarations {"kinds":[…],"relations":[…]}
--                 (domain.OntologyDoc), exactly what Apply will be given.
--   proposer      the agent principal the transport resolved ("agent:<id>").
--   base_version  the namespace's declarations version it was classified against.
--   class/changes the classification at draft time (additive | tightening and the
--                 classified differences); breaking drafts are refused, not stored.
--   reason        why, shown to the approver.
--
-- ontology_draft_events — the draft's state log, append-only too: 'pending' when
-- its approval was raised (approval_id), then at most ONE terminal state:
-- 'applied' (applied_version, actor = the approving person), 'rejected' (denied,
-- withdrawn, unroutable), 'expired', or 'superseded' (approved, but the change no
-- longer classifies as drafted over the current declarations, or no longer
-- validates). The partial unique index makes the first terminal state win.
--
-- Both tables refuse UPDATE and DELETE (the ontology_declarations precedent).
--
-- ADDITIVE. Rollback: DROP TABLE ontology_draft_events; DROP TABLE ontology_drafts;
--   DROP FUNCTION ontology_drafts_append_only();

CREATE TABLE IF NOT EXISTS ontology_drafts (
	draft_id     TEXT PRIMARY KEY,
	namespace    TEXT NOT NULL,
	change       JSONB NOT NULL,
	proposer     TEXT NOT NULL,
	base_version BIGINT NOT NULL,
	class        TEXT NOT NULL CHECK (class IN ('additive', 'tightening')),
	changes      JSONB NOT NULL,
	reason       TEXT NOT NULL DEFAULT '',
	created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_ontology_drafts_namespace
	ON ontology_drafts (namespace, created_at);

CREATE TABLE IF NOT EXISTS ontology_draft_events (
	draft_id        TEXT NOT NULL REFERENCES ontology_drafts (draft_id),
	state           TEXT NOT NULL CHECK (state IN ('pending', 'applied', 'rejected', 'expired', 'superseded')),
	approval_id     TEXT NOT NULL DEFAULT '',
	actor           TEXT NOT NULL,
	detail          TEXT NOT NULL DEFAULT '',
	applied_version BIGINT,
	at              TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (draft_id, state)
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_ontology_draft_events_terminal
	ON ontology_draft_events (draft_id) WHERE state <> 'pending';

CREATE OR REPLACE FUNCTION ontology_drafts_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% is append-only (ADR-0149 D2)', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_ontology_drafts_append_only ON ontology_drafts;
CREATE TRIGGER trg_ontology_drafts_append_only BEFORE UPDATE OR DELETE ON ontology_drafts
    FOR EACH ROW EXECUTE FUNCTION ontology_drafts_append_only();
DROP TRIGGER IF EXISTS trg_ontology_draft_events_append_only ON ontology_draft_events;
CREATE TRIGGER trg_ontology_draft_events_append_only BEFORE UPDATE OR DELETE ON ontology_draft_events
    FOR EACH ROW EXECUTE FUNCTION ontology_drafts_append_only();
