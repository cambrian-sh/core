-- 0029_skill_lifecycle.sql — ADR-0139 D13 skill lifecycle
-- (T-20261105-adr-0139-d13-skill-lifecycle-with-agent).
-- ADDITIVE: five new tables; nothing existing is changed or dropped, and older binaries
-- ignore them.
--
-- Approved agent-proposed skills live HERE, not in the repository's shipped skills/
-- directory: the kernel materialises them into the same in-memory SkillRegistry that
-- file discovery fills (internal/skill/lifecycle). Shipped skills get a skills row too
-- (source 'builtin') so they carry a version, usage telemetry and a ledger, but their
-- content is always served from disk.
--
-- Nothing in these tables is ever deleted by the kernel:
--   skill_proposals   the staged approval queue; decided rows keep their decision.
--   skill_versions    immutable content, one row per (skill, version).
--   skill_records     (NOT `skills`: that is 0007's DocTypeSkill descriptor-index table)
--                     one mutable row per skill: current version, lifecycle state, lock,
--                     usage counters. Its ledger_seq is the optimistic-concurrency token.
--   skill_ledger      APPEND-ONLY mutation history; a rollback is a new row. The id is
--                     derived from (skill_name, seq) (AGENTS.md §2.6), so two writers racing
--                     for one sequence collide on the key instead of duplicating history.
--   skill_run_uses    which run loaded which skill, settled once when the run's outcome is
--                     known (success_count).

CREATE TABLE IF NOT EXISTS skill_proposals (
    id                TEXT PRIMARY KEY,           -- sp-<hash(name, base_version, content)>
    name              TEXT NOT NULL,
    skill_md          TEXT NOT NULL,
    content_hash      TEXT NOT NULL,
    rationale         TEXT NOT NULL DEFAULT '',
    evidence_cids     TEXT[] NOT NULL DEFAULT '{}',
    base_version      TEXT NOT NULL DEFAULT '',
    state             TEXT NOT NULL CHECK (state IN ('pending', 'approved', 'rejected')),
    proposer_agent_id TEXT NOT NULL,
    principal_id      TEXT NOT NULL DEFAULT '',
    source_run_id     TEXT NOT NULL DEFAULT '',
    session_id        TEXT NOT NULL DEFAULT '',
    scan_verdict      TEXT NOT NULL,
    scan_findings     TEXT[] NOT NULL DEFAULT '{}',
    created_at        TIMESTAMPTZ NOT NULL,
    decided_at        TIMESTAMPTZ,
    decided_by        TEXT NOT NULL DEFAULT '',
    decision_note     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_skill_proposals_state ON skill_proposals (state, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_skill_proposals_name ON skill_proposals (name);
CREATE INDEX IF NOT EXISTS idx_skill_proposals_proposer ON skill_proposals (proposer_agent_id, state);

CREATE TABLE IF NOT EXISTS skill_versions (
    skill_name  TEXT NOT NULL,
    version     TEXT NOT NULL,
    skill_md    TEXT NOT NULL,
    source      TEXT NOT NULL,
    proposal_id TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (skill_name, version)
);

CREATE TABLE IF NOT EXISTS skill_records (
    name             TEXT PRIMARY KEY,
    source           TEXT NOT NULL,
    version          TEXT NOT NULL,
    state            TEXT NOT NULL CHECK (state IN ('active', 'stale', 'archived')),
    state_changed_at TIMESTAMPTZ NOT NULL,
    current_since    TIMESTAMPTZ NOT NULL,
    locked           BOOLEAN NOT NULL DEFAULT FALSE,
    use_count        BIGINT NOT NULL DEFAULT 0,
    success_count    BIGINT NOT NULL DEFAULT 0,
    last_used_at     TIMESTAMPTZ,
    ledger_entry_id  TEXT NOT NULL DEFAULT '',
    ledger_seq       BIGINT NOT NULL DEFAULT 0,
    created_at       TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS skill_ledger (
    id           TEXT PRIMARY KEY,                -- sle-<hash(skill_name, seq)>
    skill_name   TEXT NOT NULL,
    seq          BIGINT NOT NULL,
    mutation     TEXT NOT NULL,
    from_version TEXT NOT NULL DEFAULT '',
    to_version   TEXT NOT NULL DEFAULT '',
    from_state   TEXT NOT NULL DEFAULT '',
    to_state     TEXT NOT NULL DEFAULT '',
    locked       BOOLEAN NOT NULL DEFAULT FALSE,
    actor        TEXT NOT NULL,
    reason       TEXT NOT NULL DEFAULT '',
    at           TIMESTAMPTZ NOT NULL,
    proposal_id  TEXT NOT NULL DEFAULT '',
    rollback_of  TEXT NOT NULL DEFAULT '',
    command_id   TEXT NOT NULL DEFAULT '',
    UNIQUE (skill_name, seq)
);
-- An operator command_id appends at most one ledger row (idempotent retries).
CREATE UNIQUE INDEX IF NOT EXISTS idx_skill_ledger_command ON skill_ledger (command_id) WHERE command_id <> '';
CREATE INDEX IF NOT EXISTS idx_skill_ledger_at ON skill_ledger (at DESC);

-- Append-only is enforced by the database, not only by the code: UPDATE and DELETE on
-- skill_ledger raise. History is corrected by appending (a rollback), never by editing.
CREATE OR REPLACE FUNCTION skill_ledger_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'skill_ledger is append-only (ADR-0139 D13)';
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_skill_ledger_append_only ON skill_ledger;
CREATE TRIGGER trg_skill_ledger_append_only BEFORE UPDATE OR DELETE ON skill_ledger
    FOR EACH ROW EXECUTE FUNCTION skill_ledger_append_only();

CREATE TABLE IF NOT EXISTS skill_run_uses (
    run_id     TEXT NOT NULL,
    skill_name TEXT NOT NULL,
    first_used TIMESTAMPTZ NOT NULL,
    settled    BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (run_id, skill_name)
);
