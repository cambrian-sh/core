-- 0061_conversation_message_sources_backfill.sql — best-effort backfill of the sources
-- of agent replies stored before 0060, so a memory erasure reaches the old replies that
-- quoted a forgotten record too (T-20261118-forgetting-a-document-still-leaves-its-value-in,
-- ENG-2155; builds on 0060 / ENG-2109; ADR-0137 erasure inventory).
-- cambrian:not-destructive: fills conversation_messages.source_refs only where it is the empty '[]' 0060 defaulted on replies stored before it, and adds one report table; no row is deleted and no other value is rewritten.
-- ADDITIVE: older binaries ignore the report table and read the filled refs as they read any reply's.
--
-- 0060 added source_refs with the default '[]', so every reply stored before it reads
-- as resting on nothing and an erase never redacts it. A reply's sources are the
-- source_refs of the run-ledger entries of its turn (chat.TurnService.turnSourceRefs);
-- a turn's id is the person's message it answers, i.e. the latest user message before
-- the reply. Where those entries are still in ledger_entries (expired-but-unswept rows
-- included: their refs are still true), their union is written onto the reply.
--
-- A migration, not an operator command (decision logged on ENG-2155): it runs at the
-- upgrade on every install, while the ledger entries that can supply the sources are
-- still retained (execution.ledger.retention_hours); a command would run only when an
-- operator remembers it, by when they may have expired. One-shot by schema_migrations,
-- and idempotent on its own (only '[]' replies without a report row are considered).
--
-- conversation_message_source_backfill is the report — the durable log of the run,
-- one content-free row per reply considered:
--   backfilled  sources found and written (refs = how many)
--   no_sources  the turn's ledger entries are there and name no memory record (the
--               turn recalled nothing; '[]' was right)
--   unresolved  no ledger entry of the turn is left (expired and swept, or the ledger
--               was off): the reply stays '[]' and a forget will NOT redact it
-- Query: SELECT outcome, count(*) FROM conversation_message_source_backfill GROUP BY 1;
-- Rows go with their message (ON DELETE CASCADE), so the report never outlives the
-- conversation it describes.
--
-- Rollback: DROP TABLE conversation_message_source_backfill; (the filled refs are
--   correct data and may stay; to undo them:
--   UPDATE conversation_messages m SET source_refs = '[]'::jsonb
--     FROM conversation_message_source_backfill b
--    WHERE b.message_id = m.id AND b.outcome = 'backfilled' AND m.redacted_at IS NULL;)

CREATE TABLE IF NOT EXISTS conversation_message_source_backfill (
    message_id      TEXT PRIMARY KEY REFERENCES conversation_messages (id) ON DELETE CASCADE,
    conversation_id TEXT NOT NULL,
    turn_id         TEXT NOT NULL DEFAULT '',
    outcome         TEXT NOT NULL CHECK (outcome IN ('backfilled', 'no_sources', 'unresolved')),
    refs            INTEGER NOT NULL DEFAULT 0,
    backfilled_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

WITH cutoff AS (
    SELECT applied_at FROM schema_migrations WHERE version = 60
),
candidates AS (
    SELECT m.id, m.conversation_id,
           COALESCE((SELECT u.id FROM conversation_messages u
                      WHERE u.conversation_id = m.conversation_id AND u.role = 'user' AND u.seq < m.seq
                      ORDER BY u.seq DESC LIMIT 1), '') AS turn_id
      FROM conversation_messages m, cutoff
     WHERE m.role = 'agent'
       AND m.redacted_at IS NULL
       AND m.source_refs = '[]'::jsonb
       AND m.created_at < cutoff.applied_at
       AND NOT EXISTS (SELECT 1 FROM conversation_message_source_backfill b WHERE b.message_id = m.id)
),
found AS (
    SELECT c.id, c.conversation_id, c.turn_id,
           (SELECT count(*) FROM ledger_entries l
             WHERE c.turn_id <> '' AND l.conversation_id = c.conversation_id AND l.turn_id = c.turn_id) AS entries,
           (SELECT COALESCE(jsonb_agg(DISTINCT r.ref ORDER BY r.ref), '[]'::jsonb)
              FROM ledger_entries l, jsonb_array_elements_text(l.source_refs) AS r(ref)
             WHERE c.turn_id <> '' AND l.conversation_id = c.conversation_id AND l.turn_id = c.turn_id
               AND r.ref <> '') AS refs
      FROM candidates c
),
filled AS (
    UPDATE conversation_messages m
       SET source_refs = f.refs
      FROM found f
     WHERE m.id = f.id AND jsonb_array_length(f.refs) > 0
    RETURNING m.id
)
INSERT INTO conversation_message_source_backfill (message_id, conversation_id, turn_id, outcome, refs)
SELECT f.id, f.conversation_id, f.turn_id,
       CASE WHEN jsonb_array_length(f.refs) > 0 THEN 'backfilled'
            WHEN f.entries > 0 THEN 'no_sources'
            ELSE 'unresolved' END,
       jsonb_array_length(f.refs)
  FROM found f;
