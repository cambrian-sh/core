-- 0038_links_review_inbox.sql — the link review inbox's order and membership
-- (ADR-0128 §3/D4/D9; owner decisions 2026-11-12, tickets
-- T-20261112-entity-op-s-candidates-read-cuts-the-whole and
-- T-20261112-confirmed-link-candidates-stay-in-the-review).
--
-- 1. idx_links_review is rebuilt for the inbox's new order: confidence DESC, then
--    OLDEST first (asserted_at ASC), then id — the order the operator console
--    already sorted to (ui LinkReviewConsole orderCandidates). The old index
--    (namespace_id, state, confidence DESC) served only the first key. Retracted
--    candidates are never in the inbox, so the index leaves them out.
--
-- 2. idx_links_candidate_from / _to let the entity card read ONE subject's
--    candidates in SQL instead of cutting the whole inbox to a page and filtering
--    it in Go (the defect: a subject's candidates went missing behind other
--    subjects' higher-confidence ones).
--
-- 3. idx_links_confirmations serves the inbox's NOT EXISTS: a candidate that a
--    person confirmed is answered. ConfirmLink APPENDS a human row whose
--    source_ref is 'confirm:<candidate id>' and leaves the candidate untouched
--    (ADR-0128 D4, append-only); the inbox therefore excludes candidates that
--    have such a row instead of editing them. Partial on mechanism='human', the
--    only mechanism ConfirmLink writes.
--
-- ADDITIVE (indexes only). Rollback: DROP INDEX idx_links_confirmations,
--   idx_links_candidate_to, idx_links_candidate_from, idx_links_review; then
--   CREATE INDEX idx_links_review ON links (namespace_id, state, confidence DESC)
--   WHERE state = 'candidate';

DROP INDEX IF EXISTS idx_links_review;
CREATE INDEX IF NOT EXISTS idx_links_review
	ON links (namespace_id, confidence DESC, asserted_at ASC, id)
	WHERE state = 'candidate' AND retracted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_links_candidate_from ON links (namespace_id, from_ref)
	WHERE state = 'candidate' AND retracted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_links_candidate_to ON links (namespace_id, to_ref)
	WHERE state = 'candidate' AND retracted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_links_confirmations ON links (namespace_id, source_ref)
	WHERE mechanism = 'human';
