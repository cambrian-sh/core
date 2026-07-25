-- 0005_session_conversation_link.sql — link a Session back to the conversation turn that
-- ordered it (ADR-0084 D2, session Phase 5).
--
-- ADR-0084 specified "a turn that actually orders work spawns a Session — a 1:N reference,
-- never an identity", and then neither entity referenced the other. The relationship existed
-- only in prose, so nothing could answer "what did this conversation actually set in motion?"
-- — or, from the other side, "who asked for this work?".
--
-- Two columns, not one, because they answer different questions:
--   * conversation_id is CORRELATION — part of the same exchange;
--   * origin_message_id is CAUSATION — caused by THIS turn.
-- Audit needs the second; a conversation view needs the first.
--
-- Deliberately NOT a foreign key to conversations(id): a session must outlive the
-- conversation that started it. Work already done does not become un-done when its chat
-- history is deleted, and a cascade here would silently destroy execution history along with
-- a conversation.
--
-- Idempotent (IF NOT EXISTS throughout) like every migration here.

ALTER TABLE sessions
    ADD COLUMN IF NOT EXISTS conversation_id   TEXT NOT NULL DEFAULT '';

ALTER TABLE sessions
    ADD COLUMN IF NOT EXISTS origin_message_id TEXT NOT NULL DEFAULT '';

-- "Which sessions did this conversation start?" — partial, because most sessions have no
-- conversation at all and there is no reason to index the empty string.
CREATE INDEX IF NOT EXISTS idx_sessions_conversation
    ON sessions (conversation_id, created_at DESC)
    WHERE conversation_id <> '';
