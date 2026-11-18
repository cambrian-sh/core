-- 0060_conversation_message_sources.sql — what an agent reply was built from, so a
-- memory erasure reaches the replies that quoted the forgotten record
-- (T-20261118-forgetting-a-document-leaves-chat-replies-that, ENG-2109; owner decision
-- 2026-11-18, option B; ADR-0137 erasure inventory).
-- cambrian:not-destructive: two new columns with defaults and one index; nothing existing is changed or dropped.
-- ADDITIVE: older binaries ignore both columns.
--
-- A chat turn's memory recalls are run-ledger entries whose source_refs name the chunks,
-- documents and episodes they returned (ADR-0157 D5.6), and an erase deletes those
-- entries. The reply the agent composed from them was a plain transcript row with no
-- record of its sources, so the erased value stayed in conversation_messages verbatim.
--
--   source_refs  JSONB array of memory record ids (chunk, document, experience) the
--                reply rests on: the union of the source_refs of the turn's ledger
--                entries, written by the chat lane with the reply. '[]' for user,
--                system and interruption rows and for replies of turns that recalled
--                nothing. Served by the GIN index (?| lookup in the erase transaction).
--   redacted_at  set by the erase that replaced the reply's text with the redaction
--                marker; the row, its role, seq and created_at stay so the
--                conversation still reads in order. NULL for every other row.
--
-- Rollback: DROP INDEX idx_conversation_messages_source_refs;
--   ALTER TABLE conversation_messages DROP COLUMN source_refs, DROP COLUMN redacted_at;

ALTER TABLE conversation_messages ADD COLUMN IF NOT EXISTS source_refs JSONB NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE conversation_messages ADD COLUMN IF NOT EXISTS redacted_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_conversation_messages_source_refs
    ON conversation_messages USING gin (source_refs);
