-- 0022_fact_items.sql — the substrate pieces of ADR-0136 Amendment A1 (the fact layer
-- on the knowledge substrate). ADDITIVE: no existing column is changed or dropped.
--
-- A fact is a `memory_fact` knowledge item (0012). Its supersession chain is not stored
-- on the items — items are append-only — but derived by the `valid_time_supersession`
-- resolution authority from the item set, the stored assessments that cite it and the
-- predicate's cardinality profile. The tables below are exactly those inputs, plus the
-- item embedding projection the query-time fact lane searches.
--
-- 1. resolutions.valid_to — the derived valid-time upper bound of a resolution version,
--    next to the existing valid_from (A1.2 rule 6). NULL = open.
-- 2. statement_values (value_type, value_entity_id) — "every fact about subject S" reads
--    through the entity-typed `subject` value (A1.1 rejected-keyings note); the fuzzy
--    candidate read and EraseItems' ValueEntityRef join use it.
-- 3. knowledge_assessments — the substrate's FIRST assessment table (A1.3). The id is a
--    deterministic hash of (authority, authority_version, prompt_hash, sorted input item
--    ids), so the same judge over the same inputs is ONE row (ON CONFLICT DO NOTHING) and
--    a replay reads the stored verdict instead of asking again (TOKI, arXiv 2606.06240).
--    A verdict is never edited: an operator-authority assessment overrules it.
-- 4. predicate_profiles — learned cardinality as versioned DATA (A1.4): one current row
--    per (namespace, kind, predicate), the previous version closed by system_to, with the
--    assessment ids that voted as provenance. source = declared (config seed), override
--    (operator) or learned (votes).
-- 5. knowledge_item_embeddings — the item projection (A1.5), the same shape as ADR-0107's
--    chunk_embeddings (0013): an UNTYPED vector so any model dimension fits, one row per
--    (item, model). Its HNSW index is a per-dimension partial index over a cast, built
--    CONCURRENTLY by the runner's companion step (migrate.go ensureConcurrentIndexes),
--    not here: the dimension is deployment data.
-- 6. knowledge_extractions — the "this content was already run through extractor X"
--    marker that replaces memory_fact_extractions (A1.6), so a zero-fact session never
--    pays a second extraction call.
--
-- Rollback: DROP TABLE knowledge_extractions, knowledge_item_embeddings,
-- predicate_profiles, knowledge_assessments; DROP INDEX idx_statement_values_entity;
-- ALTER TABLE resolutions DROP COLUMN valid_to.

ALTER TABLE resolutions ADD COLUMN IF NOT EXISTS valid_to TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_statement_values_entity
	ON statement_values (value_type, value_entity_id)
	WHERE value_entity_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS knowledge_assessments (
	-- sha256(authority|authority_version|prompt_hash|sorted input item ids), hex
	-- (domain.AssessmentID).
	id                TEXT PRIMARY KEY,
	namespace_id      TEXT NOT NULL DEFAULT 'default',
	-- The kind assessed ('memory_fact').
	kind              TEXT NOT NULL,
	-- 'llm_contradiction' | 'cue' | 'operator'.
	authority         TEXT NOT NULL,
	-- model id + prompt version: a new model or prompt is a new judge, never a replay.
	authority_version TEXT NOT NULL,
	prompt_hash       TEXT NOT NULL,
	-- The candidate set the judge weighed.
	input_item_ids    TEXT[] NOT NULL,
	-- [{"a": item id, "b": item id, "verdict": contradictory|consistent|same_predicate|uncertain|refused}]
	verdicts          JSONB NOT NULL DEFAULT '[]'::jsonb,
	-- The model's answer, verbatim.
	raw_output        TEXT NOT NULL DEFAULT '',
	-- Equal to the inputs' classification (candidates never cross a scope key). Reads
	-- filter on it like items (ADR-0118 D2).
	classification    TEXT[] NOT NULL DEFAULT '{}',
	created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_knowledge_assessments_inputs
	ON knowledge_assessments USING gin (input_item_ids);

CREATE TABLE IF NOT EXISTS predicate_profiles (
	id                      BIGSERIAL PRIMARY KEY,
	namespace_id            TEXT NOT NULL DEFAULT 'default',
	kind                    TEXT NOT NULL,
	predicate               TEXT NOT NULL,
	cardinality             TEXT NOT NULL CHECK (cardinality IN ('single', 'multi', 'unknown')),
	source                  TEXT NOT NULL CHECK (source IN ('learned', 'override', 'declared')),
	single_votes            INT  NOT NULL DEFAULT 0,
	multi_votes             INT  NOT NULL DEFAULT 0,
	-- Provenance: the assessments whose verdicts voted. A vote is counted once per
	-- assessment id, so re-reading a stored assessment never double-counts.
	evidence_assessment_ids TEXT[] NOT NULL DEFAULT '{}',
	-- A learned 'same_predicate' canonical name — a PROPOSAL that only steers future
	-- extraction; stored items are never re-keyed.
	alias_of                TEXT NOT NULL DEFAULT '',
	set_by                  TEXT NOT NULL DEFAULT '',
	reason                  TEXT NOT NULL DEFAULT '',
	system_from             TIMESTAMPTZ NOT NULL DEFAULT now(),
	system_to               TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS predicate_profiles_current_uniq
	ON predicate_profiles (namespace_id, kind, predicate)
	WHERE system_to IS NULL;

CREATE TABLE IF NOT EXISTS knowledge_item_embeddings (
	item_id       TEXT NOT NULL REFERENCES knowledge_items(id) ON DELETE CASCADE,
	model_id      TEXT NOT NULL,
	model_version TEXT NOT NULL DEFAULT '',
	dims          INT  NOT NULL,
	embedding     vector NOT NULL,
	created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (item_id, model_id)
);

CREATE TABLE IF NOT EXISTS knowledge_extractions (
	namespace_id TEXT NOT NULL DEFAULT 'default',
	-- The extractor that ran ('memory_fact').
	extractor    TEXT NOT NULL,
	document_id  TEXT NOT NULL,
	-- Digest of the document's chunk ids in ingest order (themselves content-derived).
	content_hash TEXT NOT NULL,
	items        INT  NOT NULL DEFAULT 0,
	extracted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (namespace_id, extractor, document_id, content_hash)
);
CREATE INDEX IF NOT EXISTS idx_knowledge_extractions_document
	ON knowledge_extractions (extractor, document_id);
