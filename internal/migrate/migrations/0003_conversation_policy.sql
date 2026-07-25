-- 0003_conversation_policy.sql — optional per-conversation policy prompt (ADR-0084 D9).
--
-- The operator chat lane can open a conversation with a system/policy prompt that is threaded
-- into every turn. Stored on the conversation (set once) rather than resent per turn.
--
-- Separate migration rather than an edit to 0002 because 0002 has already been applied in
-- environments; migrations are append-only (ADR-0064). Idempotent.

ALTER TABLE conversations ADD COLUMN IF NOT EXISTS policy TEXT NOT NULL DEFAULT '';
