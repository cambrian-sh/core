-- 0006_conversation_delivery.sql — where a reply to this conversation goes (ADR-0090).
--
-- The delivery address is the ENVELOPE: which registered ingress carries the traffic, and
-- which identity on the far side of it. It is bound by the kernel on first inbound contact
-- and never supplied by an agent — an agent names a conversation, the kernel resolves the
-- address. Without that split, anything able to produce a message could choose who reads it,
-- and a fire-and-forget ingress would deliver it without question.
--
-- Both columns default to '' so every existing conversation is simply undeliverable, which is
-- correct: nothing ever arrived through an ingress for them.
--
-- Append-only per ADR-0064; idempotent.

ALTER TABLE conversations ADD COLUMN IF NOT EXISTS delivery_ingress TEXT NOT NULL DEFAULT '';
ALTER TABLE conversations ADD COLUMN IF NOT EXISTS delivery_external TEXT NOT NULL DEFAULT '';

-- Answers "which conversation is this sender already talking in?" on every inbound message,
-- which is the hot path for an ingress. Partial, because the overwhelming majority of rows in
-- a console-only deployment are unbound and would otherwise bloat the index.
CREATE INDEX IF NOT EXISTS idx_conversations_delivery
    ON conversations (delivery_ingress, delivery_external)
    WHERE delivery_ingress <> '';
