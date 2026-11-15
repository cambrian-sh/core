-- 0052_tool_grants.sql — durable operator tool grants (ENG-1116; owner decision
-- 2026-11-15, ADR-0039 / ADR-0047 D7).
-- ADDITIVE: one new table; nothing existing is changed, older binaries ignore it.
-- cambrian:not-destructive: creates one table and one index, deletes and rewrites nothing.
--
--   tool_grants  one row per (agent, tool) an OPERATOR granted through OperatorConsole
--                SetToolGrant / SetToolPolicy (incl. propose_knowledge and
--                propose_ontology_change grants), so a grant survives a kernel restart.
--                The natural key (agent_id, tool) is the primary key (AGENTS.md §2.6): a
--                retried write lands on the same row. policy is the grant's
--                domain.ToolResourcePolicy as JSON. A revoke deletes the row. Profile
--                allowlist grants are NOT stored here: they are re-derived from the
--                profile and the live tool registry on every boot.
--
-- Rollback: DROP TABLE tool_grants;

CREATE TABLE IF NOT EXISTS tool_grants (
    agent_id    TEXT NOT NULL CHECK (agent_id <> ''),
    tool        TEXT NOT NULL CHECK (tool <> ''),
    policy      JSONB NOT NULL DEFAULT '{}'::jsonb,
    granted_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (agent_id, tool)
);

CREATE INDEX IF NOT EXISTS idx_tool_grants_tool ON tool_grants (tool);
