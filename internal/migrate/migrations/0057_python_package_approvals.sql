-- 0057_python_package_approvals.sql — durable approvals of per-agent Python packages
-- (ADR-0157 D7.8.4, Amendment A1 stage P3; ENG-2009).
-- cambrian:not-destructive: two new tables and a trigger; nothing existing is changed or dropped.
-- ADDITIVE: older binaries ignore them.
--
-- An agent declares packages beyond the kernel-managed base env (a profile's or
-- AGENT_MANIFEST's python_packages). The kernel resolves each declared package + exact
-- version to a hash-pinned lock against the base env and raises one python_package card on
-- the approval hub; only an Operator answers it. Approving the card approves exactly that
-- lock for that agent (per-target scope, owner A1-Q9).
--
--   python_package_approvals one row per (target agent, package, version, lock hash); id
--                            ppa-<hash(target, name, version, lock_sha256)> (AGENTS.md §2.6).
--                            A rebuild of an approved lock raises no new card; a different
--                            lock (another dependency version or file) is a new row.
--   python_package_events    APPEND-ONLY audit of every request, decision and install;
--                            id ppe-<hash(record id, seq)>.
--
-- Rollback: DROP TABLE python_package_events, python_package_approvals;

CREATE TABLE IF NOT EXISTS python_package_approvals (
    id              TEXT PRIMARY KEY,
    target          TEXT NOT NULL CHECK (target <> ''),
    name            TEXT NOT NULL CHECK (name <> ''),
    version         TEXT NOT NULL CHECK (version <> ''),
    extras          JSONB NOT NULL DEFAULT '[]'::jsonb,
    lock_sha256     TEXT NOT NULL CHECK (lock_sha256 <> ''),
    lock            JSONB NOT NULL,
    base_generation TEXT NOT NULL DEFAULT '',
    state           TEXT NOT NULL CHECK (state IN ('pending', 'approved', 'denied', 'expired')),
    approval_id     TEXT NOT NULL DEFAULT '',
    requested_by    TEXT NOT NULL DEFAULT '',
    requested_at    TIMESTAMPTZ NOT NULL,
    decided_by      TEXT NOT NULL DEFAULT '',
    decided_at      TIMESTAMPTZ,
    detail          TEXT NOT NULL DEFAULT '',
    UNIQUE (target, name, version, lock_sha256)
);
CREATE INDEX IF NOT EXISTS idx_python_package_approvals_target ON python_package_approvals (target, requested_at DESC);
CREATE INDEX IF NOT EXISTS idx_python_package_approvals_package ON python_package_approvals (name, version);
CREATE INDEX IF NOT EXISTS idx_python_package_approvals_card ON python_package_approvals (approval_id) WHERE approval_id <> '';

CREATE TABLE IF NOT EXISTS python_package_events (
    id          TEXT PRIMARY KEY,
    record_id   TEXT NOT NULL,
    seq         BIGINT NOT NULL,
    target      TEXT NOT NULL,
    name        TEXT NOT NULL,
    version     TEXT NOT NULL,
    action      TEXT NOT NULL,
    actor       TEXT NOT NULL DEFAULT '',
    detail      TEXT NOT NULL DEFAULT '',
    generation  TEXT NOT NULL DEFAULT '',
    at          TIMESTAMPTZ NOT NULL,
    UNIQUE (record_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_python_package_events_at ON python_package_events (at DESC);

-- Append-only in the database, not only in the code.
CREATE OR REPLACE FUNCTION python_package_events_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'python_package_events is append-only (python package install audit)';
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_python_package_events_append_only ON python_package_events;
CREATE TRIGGER trg_python_package_events_append_only BEFORE UPDATE OR DELETE ON python_package_events
    FOR EACH ROW EXECUTE FUNCTION python_package_events_append_only();
