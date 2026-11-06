-- 0031_skill_grants.sql — per-agent skill access grants (owner decision 2026-11-05,
-- cambrian-knowledge/decisions/skill-access-grants.md; amends ADR-0139 D13;
-- T-20261105-approved-agent-proposed-skills-are-unreachable).
-- ADDITIVE: three new tables; nothing existing is changed or dropped, and older binaries
-- ignore them.
--
-- A profile agent's effective skill set is its profile's `skills` list PLUS its active
-- grants here. A grant is keyed by agent id (never by the profile's source hash or
-- version), so it survives profile edits, and lasts until an operator revokes it.
--
--   skill_grants          one row per (agent, skill); id sg-<hash(agent, skill)>
--                         (AGENTS.md §2.6). A revoke stamps revoked_at; a later grant
--                         clears it again on the same row.
--   skill_access_requests one row per (agent, skill): the agent's latest use_skill request
--                         for a skill outside its effective set. 'pending' stays visible
--                         to operators after the live approval timed out; granting later
--                         resolves it.
--   skill_grant_events    APPEND-ONLY audit of every grant, revoke, request and denial;
--                         id sge-<hash(agent, skill, seq)>. An operator command_id
--                         appends at most one row (idempotent retries).
--
-- Rollback: DROP TABLE skill_grant_events, skill_access_requests, skill_grants;

CREATE TABLE IF NOT EXISTS skill_grants (
    id            TEXT PRIMARY KEY,
    agent_id      TEXT NOT NULL CHECK (agent_id <> ''),
    skill_name    TEXT NOT NULL CHECK (skill_name <> ''),
    source        TEXT NOT NULL CHECK (source IN ('request', 'proposal', 'operator')),
    granted_by    TEXT NOT NULL DEFAULT '',
    granted_at    TIMESTAMPTZ NOT NULL,
    reason        TEXT NOT NULL DEFAULT '',
    revoked_at    TIMESTAMPTZ,
    revoked_by    TEXT NOT NULL DEFAULT '',
    revoke_reason TEXT NOT NULL DEFAULT '',
    UNIQUE (agent_id, skill_name)
);
CREATE INDEX IF NOT EXISTS idx_skill_grants_active ON skill_grants (agent_id) WHERE revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_skill_grants_skill ON skill_grants (skill_name);

CREATE TABLE IF NOT EXISTS skill_access_requests (
    id           TEXT PRIMARY KEY,
    agent_id     TEXT NOT NULL,
    skill_name   TEXT NOT NULL,
    run_id       TEXT NOT NULL DEFAULT '',
    rationale    TEXT NOT NULL DEFAULT '',
    state        TEXT NOT NULL CHECK (state IN ('pending', 'granted', 'denied')),
    requested_at TIMESTAMPTZ NOT NULL,
    decided_at   TIMESTAMPTZ,
    decided_by   TEXT NOT NULL DEFAULT '',
    UNIQUE (agent_id, skill_name)
);
CREATE INDEX IF NOT EXISTS idx_skill_access_requests_state ON skill_access_requests (state, requested_at DESC);

CREATE TABLE IF NOT EXISTS skill_grant_events (
    id         TEXT PRIMARY KEY,
    agent_id   TEXT NOT NULL,
    skill_name TEXT NOT NULL,
    seq        BIGINT NOT NULL,
    action     TEXT NOT NULL,
    source     TEXT NOT NULL DEFAULT '',
    actor      TEXT NOT NULL DEFAULT '',
    reason     TEXT NOT NULL DEFAULT '',
    run_id     TEXT NOT NULL DEFAULT '',
    at         TIMESTAMPTZ NOT NULL,
    command_id TEXT NOT NULL DEFAULT '',
    UNIQUE (agent_id, skill_name, seq)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_skill_grant_events_command ON skill_grant_events (command_id) WHERE command_id <> '';
CREATE INDEX IF NOT EXISTS idx_skill_grant_events_at ON skill_grant_events (at DESC);

-- Append-only in the database, not only in the code.
CREATE OR REPLACE FUNCTION skill_grant_events_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'skill_grant_events is append-only (skill access grants audit)';
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_skill_grant_events_append_only ON skill_grant_events;
CREATE TRIGGER trg_skill_grant_events_append_only BEFORE UPDATE OR DELETE ON skill_grant_events
    FOR EACH ROW EXECUTE FUNCTION skill_grant_events_append_only();
