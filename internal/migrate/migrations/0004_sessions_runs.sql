-- 0004_sessions_runs.sql — Sessions, Runs and run checkpoints (session Phase 4).
--
-- These three lived in bbolt: no schema, no indexes, no foreign keys, and — the actual
-- defect — no retention. Every Execute minted a session that nothing ever closed, and the
-- operator plane paginated over them with a full-bucket scan on every snapshot.
--
-- Moving them here buys three things bbolt could not:
--   * ON DELETE CASCADE, so purging a session reclaims its runs and their checkpoints in
--     one statement instead of three hand-written sweeps that can drift apart;
--   * an index on (status, updated_at) so "the operationally-live set" and "what is idle
--     enough to age out" are index scans rather than full scans;
--   * a checkpoint key ordered by (run_id, step_index) as INTEGERS. The bbolt key was a
--     string, so step 10 sorted before step 2 and "the latest checkpoint" was whichever
--     step happened to sort last.
--
-- Idempotent (IF NOT EXISTS throughout) like every migration here.

CREATE TABLE IF NOT EXISTS sessions (
    id           TEXT PRIMARY KEY,
    parent_id    TEXT NOT NULL DEFAULT '',
    goal         TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL,
    summary      TEXT NOT NULL DEFAULT '',
    -- caller_scope is the non-forgeable, server-side scope narrowing (ADR-0034 D13),
    -- stored as JSON because its shape is owned by the domain, not by this schema.
    caller_scope JSONB,
    created_at   TIMESTAMPTZ NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ
);

-- The two hot queries: the live set (status='active'/'paused') and the idle sweep
-- (status='active' AND updated_at < cutoff). Both are covered by this one index.
CREATE INDEX IF NOT EXISTS idx_sessions_status_updated
    ON sessions (status, updated_at DESC);

-- Retention scans completed sessions by age.
CREATE INDEX IF NOT EXISTS idx_sessions_completed_at
    ON sessions (completed_at)
    WHERE completed_at IS NOT NULL;

CREATE TABLE IF NOT EXISTS runs (
    id         TEXT PRIMARY KEY,
    -- A run's session may already have been purged; ON DELETE CASCADE means purging a
    -- session reclaims its runs, and their checkpoints in turn.
    session_id TEXT REFERENCES sessions (id) ON DELETE CASCADE,
    plan_id    TEXT NOT NULL DEFAULT '',
    subject    TEXT NOT NULL DEFAULT '',
    status     TEXT NOT NULL,
    -- plan is the executed ExecutionPlan, stored so a resume replays against the SAME
    -- steps its checkpoints were taken against. ADR-0012 §3 always specified this; without
    -- it a step index has no steps to index into and resume cannot be sound.
    plan       JSONB,
    started_at TIMESTAMPTZ NOT NULL,
    ended_at   TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_runs_session_started
    ON runs (session_id, started_at);

CREATE TABLE IF NOT EXISTS run_checkpoints (
    run_id     TEXT NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    -- INTEGER, not text: ordering by step is numeric here by construction.
    step_index INTEGER NOT NULL,
    context    JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (run_id, step_index)
);
