-- 0066_approval_notice_action_history.sql — the approver's duplicate notice and the
-- conversation action history survive a kernel restart
-- (T-20261107-approval-duplicate-notice-and-action-history, ENG-784; owner decision
-- 2026-11-19: the additive migration is approved).
-- cambrian:not-destructive: one new column with a constant default and one new table; nothing existing is changed or dropped.
-- ADDITIVE: older binaries ignore both.
--
-- An approval for a side-effecting call carries a notice naming the earlier calls of its
-- conversation it may duplicate (domain/action_history.go). The notice lived only in the
-- hub's memory and the history only in the executor's, so after a restart a pending item
-- listed without its notice and the conversation's earlier writes were forgotten.
--
--   approvals.duplicate_notice  ApprovalItem.DuplicateNotice; '' = none (and every row
--                               raised before 0066).
--   action_history              the completed side-effecting calls of a conversation (or
--                               task session) — only completed ones: a pending call is
--                               re-raised by approval recovery, a running one died with
--                               the process. key is content-derived (scope, tool,
--                               canonical arguments, approval, returned ids), never a
--                               clock. Rows older than the notice window
--                               (execution.tools.duplicate_notice_window_minutes) are
--                               deleted on every write and never read, so the arguments
--                               held here are bounded by that window (default 24 h).
--
-- IDEMPOTENT (IF NOT EXISTS). Rollback: DROP TABLE IF EXISTS action_history;
-- ALTER TABLE approvals DROP COLUMN IF EXISTS duplicate_notice;

ALTER TABLE approvals ADD COLUMN IF NOT EXISTS duplicate_notice TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS action_history (
    key         TEXT PRIMARY KEY,
    scope       TEXT NOT NULL,
    tool        TEXT NOT NULL,
    args        BYTEA NOT NULL,
    approval_id TEXT NOT NULL DEFAULT '',
    ids         JSONB NOT NULL DEFAULT '[]'::jsonb,
    done_at     TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_action_history_scope ON action_history (scope, done_at);
CREATE INDEX IF NOT EXISTS idx_action_history_done_at ON action_history (done_at);
