-- 0002_conversations.sql — Conversation + Message model (ADR-0084 D1).
--
-- Chat state moves out of process memory and into the kernel. The premium chat manager
-- previously held transcripts in a Go map that died on restart, and the OSS kernel had no
-- message entity at all, so neither lane could offer resumable conversations.
--
-- Idempotent (IF NOT EXISTS throughout) like every migration here.

CREATE TABLE IF NOT EXISTS conversations (
    id         TEXT PRIMARY KEY,
    owner_id   TEXT NOT NULL,
    title      TEXT NOT NULL DEFAULT '',
    status     TEXT NOT NULL,
    profile    TEXT NOT NULL,
    -- next_seq is the per-conversation message counter. AppendMessage bumps it under the
    -- row lock and uses the prior value, which makes Seq assignment atomic without a
    -- read-modify-write race between concurrent turns.
    next_seq   BIGINT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

-- Owner listing: "my conversations, most recent first".
CREATE INDEX IF NOT EXISTS idx_conversations_owner
    ON conversations (owner_id, updated_at DESC);

CREATE TABLE IF NOT EXISTS conversation_messages (
    id              TEXT PRIMARY KEY,
    conversation_id TEXT NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    seq             BIGINT NOT NULL,
    role            TEXT NOT NULL,
    content         TEXT NOT NULL,
    -- client_id is an optional caller idempotency key; see the partial unique index below.
    client_id       TEXT,
    created_at      TIMESTAMPTZ NOT NULL,
    -- Ordering is unique per conversation: two turns can never claim the same position.
    UNIQUE (conversation_id, seq)
);

-- History reads are always "conversation, in order, after N".
CREATE INDEX IF NOT EXISTS idx_conversation_messages_order
    ON conversation_messages (conversation_id, seq);

-- Retry idempotency: a client resending the same turn must not duplicate it. Partial, so
-- the common case (no client_id) is unconstrained rather than colliding on NULL/''.
CREATE UNIQUE INDEX IF NOT EXISTS uq_conversation_messages_client
    ON conversation_messages (conversation_id, client_id)
    WHERE client_id IS NOT NULL AND client_id <> '';
