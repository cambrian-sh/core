-- 0010_experiences.sql — the memory nobody ingested gets a parent row (ADR-0095).
--
-- ADR-0093 split one overloaded table into six and named the decision that mattered:
-- "A document was not an entity." It then reasoned that agent-written memories have no
-- parent, so `chunks.document_id` is nullable and they keep a NULL.
--
-- That was right about `documents` and wrong about parentage in general. An agent-written
-- memory does have a parent — not a document, but the EPISODE that produced it. Under
-- ADR-0049 every experiential record is minted by a plan execution, and `plan_id` is
-- already stamped in chunk metadata; the parent existed in the data and had no row.
--
-- Three things were impossible without it, in ascending order of seriousness:
--   1. You could not LIST what the system had done — no row to select.
--   2. You could not DELETE an experience — rows scattered with no cascade, so tenant
--      offboarding, retention and active-forgetting were inexpressible.
--   3. You could not GOVERN one. ADR-0091 shipped deny-by-default closed tags and verified
--      "from chat:airline -> internal memory: denied", but it also recorded the prerequisite
--      it had to fix for MCP first: an untagged resource has no tags for any predicate to
--      act on, so "the boundary was inexpressible regardless of the policy written."
--      Experiential memory was in exactly that state.
--
-- This migration is ADDITIVE, per ADR-0093 D6's standing rule: the corpus is the shared
-- store the benchmarks measure against, and it is never destroyed to make a schema tidy.
-- Nothing is renamed, nothing is dropped, no existing row is rewritten. Rollback is
-- `DROP TABLE experiences; ALTER TABLE chunks DROP COLUMN experience_id;` plus dropping
-- experience_derivations.

-- One row per EPISODE (one plan execution). The grain matches ADR-0049 D5's one-scene-per-
-- plan, so a scene, its action path and its outcome record share exactly one parent.
CREATE TABLE IF NOT EXISTS experiences (
	id                  TEXT PRIMARY KEY,
	session_id          TEXT,
	-- The ingress the producing session was opened on (ADR-0090 Session.Surface, decided
	-- ONCE by the ingress). Half of the born-tagged stamp in ADR-0095 D4.
	surface             TEXT NOT NULL DEFAULT '',
	-- AUTHORITATIVE classification. This is the row policy attaches to, mirroring
	-- documents.tags (ADR-0093 D4). The per-chunk copy is a derived cache with ONE writer.
	tags                TEXT[] NOT NULL DEFAULT '{}',
	outcome             VARCHAR(16) NOT NULL DEFAULT 'unknown',
	metadata            JSONB,
	started_at          TIMESTAMP,
	completed_at        TIMESTAMP,
	created_at          TIMESTAMP NOT NULL DEFAULT NOW()
);

-- Policy filters on tags; same GIN shape as documents.tags.
CREATE INDEX IF NOT EXISTS idx_experiences_tags ON experiences USING GIN (tags);
CREATE INDEX IF NOT EXISTS idx_experiences_session ON experiences (session_id);
CREATE INDEX IF NOT EXISTS idx_experiences_surface ON experiences (surface);

-- A second nullable parent alongside document_id. Nullable because a DERIVED artifact
-- (a procedure induced from many episodes, a session narrative summarising many) has no
-- single parent, and forcing one would be the lie ADR-0093 D3 refused. Those link through
-- experience_derivations instead.
--
-- ON DELETE CASCADE for ADR-0093 D4's reason: an orphaned chunk of a deleted parent is
-- unreachable data that still answers searches.
ALTER TABLE chunks ADD COLUMN IF NOT EXISTS experience_id TEXT
	REFERENCES experiences(id) ON DELETE CASCADE;

CREATE INDEX IF NOT EXISTS idx_chunks_experience_id ON chunks (experience_id);

-- Provenance for artifacts distilled from N episodes (ADR-0095 D5). One table for both
-- procedures (ADR-0094) and session narratives (ADR-0029), because they are the same shape.
-- It is also what makes ADR-0094 D6 / ADR-0095 D9 checkable rather than implied: the
-- sources of a derived artifact are enumerable, so "was this derived across a closed-tag
-- boundary?" is a query rather than a belief.
CREATE TABLE IF NOT EXISTS experience_derivations (
	derived_chunk_id TEXT NOT NULL REFERENCES chunks(id) ON DELETE CASCADE,
	experience_id    TEXT NOT NULL REFERENCES experiences(id) ON DELETE CASCADE,
	PRIMARY KEY (derived_chunk_id, experience_id)
);

CREATE INDEX IF NOT EXISTS idx_experience_derivations_experience
	ON experience_derivations (experience_id);

-- NOTE — deliberately NOT done here.
--
-- No back-fill of experience_id from metadata->>'plan_id'. ADR-0095 D8 describes
-- reconstructing parents from the only record that an episode ever existed, and that is
-- correct — but the experiential WRITE PATH is currently unwired (removed 2026-07-18,
-- ADR-0049 A2.0) and its redesign is gated on a memory benchmark (A2.7 Phase 0). Writing a
-- back-fill now would invent parents for records whose shape is about to change, on a
-- shared store the benchmarks measure against. The table lands empty; the back-fill ships
-- with the writers that populate it.
--
-- Also NOT done: the trap ADR-0093 recorded. `update_activation_strength` and
-- `apply_ebbinghaus_decay` are plpgsql and resolve table names at CALL time, so 0007 had to
-- redefine them after renaming `documents`. This migration only ADDS a table and a column
-- and renames nothing, so both functions still resolve to `chunks` and are untouched.
-- Stated explicitly because the failure mode is silent — every call succeeding, nothing
-- decaying, no error anywhere — and the next person should not have to re-derive that it
-- was considered.
