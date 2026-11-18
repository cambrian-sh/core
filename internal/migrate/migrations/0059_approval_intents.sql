-- 0059_approval_intents.sql — what an approved request does, stored with its approval
-- (T-20261118-a-sync-approval-orphaned-by-a-kernel-restart; owner decision 2026-11-18,
-- option A: an approval approved after a kernel restart carries out its action).
-- cambrian:not-destructive: one new nullable column; nothing existing is changed or dropped.
-- ADDITIVE: older binaries ignore it.
--
-- A blocking approval kept everything needed to act in its requester's goroutine. After a
-- restart the pending row was still listed, and an approve recorded "approved" while
-- nothing could run it any more. The requester now stores its intent with the row: the
-- executor that carries it out ('tool', 'schedule'), the agent, the tool, the full
-- validated arguments and the kernel-held request facts (the person the call is for, the
-- ordering conversation and session, the surface, the plan step). An approve that lands
-- after the requester's kernel run ended is carried out from it, once (the ADR-0154 action
-- record approval-recovery:<id>), under the rules in force then — or refused with the
-- reason.
--
--   intent  JSON text of domain.ApprovalIntent, or NULL (async items, kinds nothing can
--           carry out after a restart, and every row once it is resolved and carried out
--           or refused). TEXT, not JSONB: the arguments are re-read byte for byte.
--
-- Rollback: ALTER TABLE approvals DROP COLUMN intent;

ALTER TABLE approvals ADD COLUMN IF NOT EXISTS intent TEXT;
CREATE INDEX IF NOT EXISTS idx_approvals_recovering ON approvals (id) WHERE state = 'approved' AND intent IS NOT NULL;
