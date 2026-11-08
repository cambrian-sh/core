-- 0033_approvals.sql — the one addressed approval hub (ADR-0142 D11; owner
-- acceptance 2026-11-06, item 1: one-time and standing approvals, approver rules).
-- ADDITIVE: two new tables; nothing existing is changed or dropped, and older
-- binaries ignore them.
--
--   approvals           one row per approval request round. id apr-<hash(natural key,
--                       round)> (AGENTS.md §2.6): a request retried after a restart
--                       finds the same row, and ids never repeat across boots the way
--                       the old in-memory appr-<counter> did. A pending row survives
--                       a restart; the next process expires it once its window passes
--                       (fail-closed) or hands a decision recorded while nobody waited
--                       to the next identical request (consumed = true once taken).
--   standing_approvals  one row per (action class, resource, scope); id
--                       stand-<hash(class, resource, scope)>. Re-granting replaces;
--                       revoking stamps revoked_at. A standing approval pre-approves
--                       later requests of the same key until revoked or expired.
--
-- rules is the approver rule set the policy resolved AT RAISE TIME, stored so the
-- rule shown and the rule enforced at answer time are the same; it is JSON
-- [{"kind":"self"},{"kind":"role","value":"compliance"}].
--
-- Rollback: DROP TABLE standing_approvals, approvals;

CREATE TABLE IF NOT EXISTS approvals (
    id              TEXT PRIMARY KEY,
    natural_key     TEXT NOT NULL,
    round           INTEGER NOT NULL DEFAULT 0,
    kind            TEXT NOT NULL DEFAULT '',
    action_class    TEXT NOT NULL DEFAULT '',
    resource        TEXT NOT NULL DEFAULT '',
    scope           TEXT NOT NULL DEFAULT '',
    agent_id        TEXT NOT NULL DEFAULT '',
    tool_name       TEXT NOT NULL DEFAULT '',
    args_preview    TEXT NOT NULL DEFAULT '',
    object          TEXT NOT NULL DEFAULT '',
    machine         TEXT NOT NULL DEFAULT '',
    candidates      JSONB NOT NULL DEFAULT '[]'::jsonb,
    beneficiary_id  TEXT NOT NULL DEFAULT '',
    beneficiary_kind TEXT NOT NULL DEFAULT '',
    conversation_id TEXT NOT NULL DEFAULT '',
    task_id         TEXT NOT NULL DEFAULT '',
    session_id      TEXT NOT NULL DEFAULT '',
    rules           JSONB NOT NULL DEFAULT '[]'::jsonb,
    allow_standing  BOOLEAN NOT NULL DEFAULT FALSE,
    never_self      BOOLEAN NOT NULL DEFAULT FALSE,
    async           BOOLEAN NOT NULL DEFAULT FALSE,
    state           TEXT NOT NULL CHECK (state IN
                      ('pending','approved','denied','expired','withdrawn','needs_hierarchy','unroutable')),
    raised_at       TIMESTAMPTZ NOT NULL,
    expires_at      TIMESTAMPTZ,
    resolved_at     TIMESTAMPTZ,
    resolved_by     TEXT NOT NULL DEFAULT '',
    choice          TEXT NOT NULL DEFAULT '',
    standing_id     TEXT NOT NULL DEFAULT '',
    detail          TEXT NOT NULL DEFAULT '',
    consumed        BOOLEAN NOT NULL DEFAULT FALSE,
    UNIQUE (natural_key, round)
);
CREATE INDEX IF NOT EXISTS idx_approvals_pending ON approvals (expires_at) WHERE state = 'pending';
CREATE INDEX IF NOT EXISTS idx_approvals_key ON approvals (natural_key, round DESC);
CREATE INDEX IF NOT EXISTS idx_approvals_raised ON approvals (raised_at DESC);

CREATE TABLE IF NOT EXISTS standing_approvals (
    id               TEXT PRIMARY KEY,
    action_class     TEXT NOT NULL CHECK (action_class <> ''),
    resource         TEXT NOT NULL DEFAULT '',
    scope            TEXT NOT NULL DEFAULT '',
    beneficiary_id   TEXT NOT NULL DEFAULT '',
    beneficiary_kind TEXT NOT NULL DEFAULT '',
    granted_by_id    TEXT NOT NULL CHECK (granted_by_id <> ''),
    granted_by_kind  TEXT NOT NULL DEFAULT '',
    granted_by_roles JSONB NOT NULL DEFAULT '[]'::jsonb,
    granted_at       TIMESTAMPTZ NOT NULL,
    expires_at       TIMESTAMPTZ,
    revoked_at       TIMESTAMPTZ,
    revoked_by       TEXT NOT NULL DEFAULT '',
    reason           TEXT NOT NULL DEFAULT '',
    from_approval    TEXT NOT NULL DEFAULT '',
    last_used_at     TIMESTAMPTZ,
    use_count        INTEGER NOT NULL DEFAULT 0,
    UNIQUE (action_class, resource, scope)
);
CREATE INDEX IF NOT EXISTS idx_standing_approvals_scope ON standing_approvals (scope) WHERE revoked_at IS NULL;
