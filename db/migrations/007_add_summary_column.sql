-- Migration 007: Add the summary column to documents (ADR-0048 #1).
--
-- summary is a one-line descriptor of a document's full `text`. When set, agent
-- recall (QueryMemory) returns the summary as the agent-facing surface — and it is
-- what gets embedded — so a large LTM fact is represented by its gist, not its full
-- body. The full content stays in `text` and, when offloaded to the ContentStore,
-- behind metadata->>'content_cid' for drill-down via get_context_node.
--
-- Mirrors the idempotent ALTER in PgVectorAdapter.ensureSchema (boot-time safety
-- net); this file is the canonical, ordered migration record.
--
-- Idempotent: safe to run twice (ADD COLUMN IF NOT EXISTS).

ALTER TABLE documents ADD COLUMN IF NOT EXISTS summary TEXT NOT NULL DEFAULT '';
