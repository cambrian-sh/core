-- 0039_evidence_outbox_retry.sql — attempts, backoff, not-ready holds and a dead
-- state for the evidence outbox (ADR-0105 D3 / ADR-0108 D3 / ADR-0112 amendment;
-- owner decision 2026-11-12, T-20261112-evidence-outbox-is-head-of-line-blocked-the).
--
-- The outbox had only processed_at, and the consumer read the oldest 64
-- unprocessed rows every tick with no cursor: 64 items that failed every time (a
-- paused ingress, a permanent refusal) stopped ingestion for every source.
--
--   attempts         failed attempts so far. A NOT-READY hold is not a failure
--                    and does not count.
--   next_attempt_at  when the item is next due; NULL = due now (every row written
--                    before this migration, and every fresh row).
--   last_error       the last failure's text, shown on a dead item.
--   dead_at          set after the 5th failed attempt: terminal, never retried
--                    automatically, listed by Ingress Studio's
--                    ListDeliveryFailures and still counted in the substrate
--                    overview's outbox_pending (it is not processed).
--   held_key         non-empty while a not-ready item is parked (e.g. the
--                    evidence source id of a paused ingress); a release names the
--                    key and makes those items due again.
--
-- The pending read is "not processed, not dead, due, oldest first". It reads two
-- partial indexes so neither a long backoff tail nor a large held backlog is
-- scanned on every poll: idx_evidence_outbox_ready (fresh and retrying rows, in
-- created_at order) and idx_evidence_outbox_held_due (held rows, by recheck time);
-- idx_evidence_outbox_held serves a release by key.
--
-- ADDITIVE. Rollback: DROP INDEX idx_evidence_outbox_dead, idx_evidence_outbox_held_due, idx_evidence_outbox_held,
--   idx_evidence_outbox_ready; CREATE INDEX idx_evidence_outbox_pending ON
--   evidence_outbox (created_at) WHERE processed_at IS NULL; ALTER TABLE
--   evidence_outbox DROP COLUMN held_key, DROP COLUMN dead_at, DROP COLUMN
--   last_error, DROP COLUMN next_attempt_at, DROP COLUMN attempts;

ALTER TABLE evidence_outbox ADD COLUMN IF NOT EXISTS attempts INT NOT NULL DEFAULT 0;
ALTER TABLE evidence_outbox ADD COLUMN IF NOT EXISTS next_attempt_at TIMESTAMPTZ;
ALTER TABLE evidence_outbox ADD COLUMN IF NOT EXISTS last_error TEXT NOT NULL DEFAULT '';
ALTER TABLE evidence_outbox ADD COLUMN IF NOT EXISTS dead_at TIMESTAMPTZ;
ALTER TABLE evidence_outbox ADD COLUMN IF NOT EXISTS held_key TEXT NOT NULL DEFAULT '';

DROP INDEX IF EXISTS idx_evidence_outbox_pending;
CREATE INDEX IF NOT EXISTS idx_evidence_outbox_ready
	ON evidence_outbox (created_at, id)
	WHERE processed_at IS NULL AND dead_at IS NULL AND held_key = '';
CREATE INDEX IF NOT EXISTS idx_evidence_outbox_held
	ON evidence_outbox (held_key, next_attempt_at)
	WHERE processed_at IS NULL AND dead_at IS NULL AND held_key <> '';
CREATE INDEX IF NOT EXISTS idx_evidence_outbox_held_due
	ON evidence_outbox (next_attempt_at)
	WHERE processed_at IS NULL AND dead_at IS NULL AND held_key <> '';
CREATE INDEX IF NOT EXISTS idx_evidence_outbox_dead
	ON evidence_outbox (dead_at DESC)
	WHERE processed_at IS NULL AND dead_at IS NOT NULL;
