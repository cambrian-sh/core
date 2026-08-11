-- 0015_evidence_parties.sql — row-level entitlement (ADR-0121).
--
-- WHO THE RECORD IS ABOUT, beside WHAT IT IS.
--
-- `classification` answers "what kind of thing is this", and the access model has only ever
-- been able to ask that — a predicate over it cannot express "the orders that are YOURS",
-- because the reader does not appear in it. This column carries the other half: the parties of
-- the record, so a policy can say "…and only the rows you are a party to" and the predicate can
-- test it in SQL (ADR-0121 D1/D2).
--
-- A FACT ABOUT THE RECORD, NOT A POLICY (INV-4, no tattooing). This order is for customer C-9
-- in exactly the sense that it is an order — neither statement says who may read it. Remove
-- every party-scoped policy and this column keeps its values, nothing consults them, and access
-- reverts precisely to what the tag terms allow. Nothing was tattooed because no policy was
-- written down.
--
-- TEXT[] with GIN, matching `classification` deliberately: the predicate is an array overlap
-- (`parties && $identities`), which is the same shape and the same index the tag terms already
-- use, so the filtered-vector-search plan stays EXACT (SUB-00) rather than degrading to a scan.
--
-- Identities are NOT vocabulary-checked, unlike classification tags (ADR-0085 D11). A
-- deployment cannot enumerate every customer in a controlled vocabulary, and requiring it to
-- would be the tag-per-customer scheme ADR-0121 exists to avoid. These are identity, not
-- classification — the distinction ADR-0099 already drew.
--
-- ADDITIVE, and empty is the safe default: a row with no parties is a row nobody is a party to,
-- so a party-scoped policy admits none of them. Every pre-existing row is therefore unchanged
-- under every policy that exists today, and only becomes restricted when an operator writes a
-- party-scoped grant.
--
-- Rollback: DROP INDEX idx_evidence_parties; ALTER TABLE evidence DROP COLUMN parties;

ALTER TABLE evidence ADD COLUMN IF NOT EXISTS parties TEXT[] NOT NULL DEFAULT '{}';

-- GIN, because every read of this column is an overlap test against a small set. A btree would
-- not serve `&&` at all.
CREATE INDEX IF NOT EXISTS idx_evidence_parties ON evidence USING GIN (parties);
