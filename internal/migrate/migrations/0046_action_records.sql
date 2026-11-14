-- 0046_action_records.sql — the one durable action record (ADR-0154; owner
-- decision 2026-11-14, ENG-812 + ENG-1428).
-- ADDITIVE: one new table; nothing existing is changed, older binaries ignore it.
--
--   action_records  one row per side-effecting action. key is deterministic from
--                   the action's natural key (AGENTS.md §2.6) — for a plan-step call
--                   the ADR-0145 D9 IdempotencyKey(run, step, tool, canonical args) —
--                   so a retry after a restart finds the same row. state walks the
--                   forward-only lifecycle proposed → approved → dispatched →
--                   accepted → confirmed | not_confirmed (the adapter checks the
--                   transition; the CHECK pins the vocabulary). result is the tool's
--                   response once accepted (replayed to a retry by the same key),
--                   error the last dispatch error (the row stays dispatched).
--                   args_hash only — never the arguments.
--
-- Rollback: DROP TABLE action_records;

CREATE TABLE IF NOT EXISTS action_records (
    key         TEXT PRIMARY KEY,
    tool        TEXT NOT NULL DEFAULT '',
    run_id      TEXT NOT NULL DEFAULT '',
    step_index  INTEGER NOT NULL DEFAULT -1,
    principal   TEXT NOT NULL DEFAULT '',
    session_id  TEXT NOT NULL DEFAULT '',
    args_hash   TEXT NOT NULL DEFAULT '',
    state       TEXT NOT NULL DEFAULT 'proposed'
                CHECK (state IN ('proposed','approved','dispatched','accepted','confirmed','not_confirmed')),
    result      JSONB,
    error       TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_action_records_run ON action_records (run_id) WHERE run_id <> '';
CREATE INDEX IF NOT EXISTS idx_action_records_state ON action_records (state, updated_at);
