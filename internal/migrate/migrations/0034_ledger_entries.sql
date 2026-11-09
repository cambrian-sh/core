-- 0034_ledger_entries.sql — the run ledger (ADR-0145 D1).
-- ADDITIVE: one new table; nothing existing is changed or dropped, and older
-- binaries ignore it.
--
--   ledger_entries  one row per tool result, step output, calculation, promoted
--                   artifact or turn scratch. The BODY is not here: it lives in the
--                   artifact vault's content-addressed byte store (ADR-0012 D7),
--                   named by body_hash; this table is the metadata beside it.
--
-- id is led_<sha256(conversation, turn, run, step, call key, content hash)> (AGENTS.md
-- §2.6), so a retried write lands on the same row (INSERT ... ON CONFLICT DO NOTHING).
-- tags is the kernel-derived write classification (authz.ClassifyArtifactWrite);
-- every read applies the reader's predicate to it in authz.EnforcingLedger.
-- search_text is the head of a text body (plus tool and arguments) for ledger_find.
-- expires_at is created_at + execution.ledger.retention_hours; the sweeper deletes
-- expired rows, and scratch rows go when their turn ends.
--
-- Rollback: DROP TABLE ledger_entries;

CREATE TABLE IF NOT EXISTS ledger_entries (
    id              TEXT PRIMARY KEY,
    kind            TEXT NOT NULL CHECK (kind IN ('tool_result','step_output','computed','artifact','scratch')),
    conversation_id TEXT NOT NULL DEFAULT '',
    turn_id         TEXT NOT NULL DEFAULT '',
    run_id          TEXT NOT NULL DEFAULT '',
    step_index      INTEGER NOT NULL DEFAULT -1,
    tool_name       TEXT NOT NULL DEFAULT '',
    args            JSONB,
    writer          TEXT NOT NULL DEFAULT '',
    owner           TEXT NOT NULL DEFAULT '',
    tags            JSONB NOT NULL DEFAULT '[]'::jsonb,
    content_type    TEXT NOT NULL DEFAULT '',
    body_hash       TEXT NOT NULL DEFAULT '',
    size_bytes      BIGINT NOT NULL DEFAULT 0,
    truncated       BOOLEAN NOT NULL DEFAULT FALSE,
    preview         TEXT NOT NULL DEFAULT '',
    content_hash    TEXT NOT NULL DEFAULT '',
    search_text     TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL,
    expires_at      TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_ledger_entries_run ON ledger_entries (run_id, step_index) WHERE run_id <> '';
CREATE INDEX IF NOT EXISTS idx_ledger_entries_turn ON ledger_entries (turn_id) WHERE turn_id <> '';
CREATE INDEX IF NOT EXISTS idx_ledger_entries_conversation ON ledger_entries (conversation_id, created_at) WHERE conversation_id <> '';
CREATE INDEX IF NOT EXISTS idx_ledger_entries_expires ON ledger_entries (expires_at);
CREATE INDEX IF NOT EXISTS idx_ledger_entries_body ON ledger_entries (body_hash);
