-- 0032_principal_registry.sql — the principal registry (ADR-0142 D3, stage 1;
-- T-20261106-identity-stage-1-principal-registry-one-user).
-- ADDITIVE: new tables only; nothing existing is changed or dropped, and older
-- binaries ignore them.
--
-- Until now Cambrian had no principal table: a person existed because something
-- REFERENCED them. A reference cannot carry a lifecycle (suspension, deprovisioning,
-- device ownership, approval addressing), so persons get rows here. Agents, daemons
-- and machines are NOT persons and keep their own registries.
--
--   principals                  one row per person (kind 'user'; 'actor' reserved, D4).
--                               id is DETERMINISTIC from the natural key (AGENTS.md §2.6):
--                               an existing referenced person keeps its bare id; a local
--                               account its username; an OIDC first login
--                               oidc-<hash(iss, sub)>. Never a clock.
--   principal_identities        (issuer, subject) → person. The pair is the identity —
--                               never the email alone (emails are reassigned).
--   principal_aliases           legacy principal strings that name a person, e.g.
--                               'agent:alice' (how a bound person ran before ADR-0142),
--                               so audit queries match both forms in the migration window.
--   principal_local_credentials password hashes of local accounts (D1.4). argon2id PHC
--                               strings; the plaintext is never stored.
--
-- Rollback: DROP TABLE principal_local_credentials, principal_aliases,
--           principal_identities, principals;

CREATE TABLE IF NOT EXISTS principals (
    id           TEXT PRIMARY KEY CHECK (id <> ''),
    kind         TEXT NOT NULL DEFAULT 'user' CHECK (kind IN ('user', 'actor')),
    display_name TEXT NOT NULL DEFAULT '',
    email        TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'active'
                 CHECK (status IN ('active', 'suspended', 'deprovisioned')),
    source       TEXT NOT NULL CHECK (source IN ('local', 'oidc', 'scim', 'directory', 'editor', 'import')),
    external_id  TEXT NOT NULL DEFAULT '',
    roles        TEXT[] NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_principals_status ON principals (status);

CREATE TABLE IF NOT EXISTS principal_identities (
    issuer       TEXT NOT NULL CHECK (issuer <> ''),
    subject      TEXT NOT NULL CHECK (subject <> ''),
    principal_id TEXT NOT NULL REFERENCES principals (id) ON DELETE CASCADE,
    email        TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (issuer, subject)
);
CREATE INDEX IF NOT EXISTS idx_principal_identities_principal ON principal_identities (principal_id);

CREATE TABLE IF NOT EXISTS principal_aliases (
    alias        TEXT PRIMARY KEY CHECK (alias <> ''),
    principal_id TEXT NOT NULL REFERENCES principals (id) ON DELETE CASCADE,
    created_at   TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_principal_aliases_principal ON principal_aliases (principal_id);

CREATE TABLE IF NOT EXISTS principal_local_credentials (
    principal_id  TEXT PRIMARY KEY REFERENCES principals (id) ON DELETE CASCADE,
    username      TEXT NOT NULL UNIQUE CHECK (username <> ''),
    password_hash TEXT NOT NULL CHECK (password_hash <> ''),
    updated_at    TIMESTAMPTZ NOT NULL
);
