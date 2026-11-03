-- 0020_memory_facts.sql — the fact layer (ADR-0136 D4) and its valid-time chains (D5).
--
-- A fact is a KEY into the chunk it came from, never the evidence itself (K = V + fact):
-- every row points back at its source chunk and source document, and retrieval returns
-- that chunk. Facts are high-volume machine extractions, which is why they live here and
-- not in `links` (ADR-0128; owner-assumed Q5): they share links' discipline — two clocks,
-- retract never delete — without its review lane.
--
-- TWO CLOCKS, as domain.Link:
--   * valid       — tstzrange [from, to): when the statement held in the world.
--                   valid_from_source says whether the lower bound was stated, inferred
--                   from the session time (single attributes only), or unknown.
--   * recorded_at / expired_at — when Cambrian learned it, and when it stopped treating
--                   it as current.
--
-- NOTHING IS EVER DELETED by supersession. A superseded fact keeps its row with a closed
-- interval and superseded_by naming its successor; a wrong extraction is retracted
-- (retracted_at). Erasure — removing content on a principal's request — is a different
-- act (T-20261103-build-a-principal-initiated-memory-erasure-path); source_chunk_id and
-- source_document_id are indexed so it can find every fact of a chunk or document.
-- There is deliberately no FK to chunks: whether erasing a chunk cascades to, retracts,
-- or re-opens a supersession chain is that ADR's Q5, and a cascade chosen here would
-- decide it silently.
--
-- CLASSIFICATION: `metadata` carries {"tags": [...]} — the source chunk's classification,
-- copied at write time through the write chokepoint — in the SAME shape chunks use, so the
-- read predicate is the one SQL mirror every other table binds on
-- (pgvector_adapter.go scopeExpressionsOn). scope_key is a digest of those tags:
-- supersession only relates facts of one classification.
--
-- THE OPEN-INTERVAL INVARIANT (D5, owner-assumed Q7): for a single-cardinality attribute,
-- one chain holds at most one value at any instant. EXCLUDE USING gist with btree_gist
-- works on every Postgres this project runs (PG 18's WITHOUT OVERLAPS is not required).
-- Restatements of a current value (corroborates) and retracted rows are outside it.
--
-- ${EMBEDDING_DIM}: the same dimension as chunks.embedding (bge-large = 1024 in
-- production; CI migrates with 1536). The HNSW index is built here: the table is new and
-- empty, so the build costs nothing inside this file's transaction.
--
-- ADDITIVE. Rollback: DROP TABLE memory_fact_extractions; DROP TABLE memory_facts;
-- (btree_gist is left installed; it is inert.)

CREATE EXTENSION IF NOT EXISTS btree_gist;

CREATE TABLE IF NOT EXISTS memory_facts (
	-- fact:<sha256(source_chunk_id|subject_key|attribute_key|value_norm)> (domain.FactID).
	id                 TEXT PRIMARY KEY,
	subject            TEXT NOT NULL,
	attribute          TEXT NOT NULL,
	value              TEXT NOT NULL,
	subject_key        TEXT NOT NULL,
	attribute_key      TEXT NOT NULL,
	value_norm         TEXT NOT NULL,
	valid              TSTZRANGE NOT NULL DEFAULT tstzrange(NULL, NULL, '[)'),
	-- stated | session | unknown (domain.FactValidFromSource).
	valid_from_source  TEXT NOT NULL DEFAULT 'unknown',
	recorded_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
	expired_at         TIMESTAMPTZ,
	superseded_by      TEXT,
	retracted_at       TIMESTAMPTZ,
	corroborates       TEXT,
	cardinality        TEXT NOT NULL DEFAULT 'multi' CHECK (cardinality IN ('single', 'multi')),
	polarity           TEXT NOT NULL DEFAULT 'positive' CHECK (polarity IN ('positive', 'negative')),
	turn_index         INTEGER,
	quote              TEXT NOT NULL DEFAULT '',
	source_chunk_id    TEXT NOT NULL,
	source_document_id TEXT NOT NULL,
	scope_key          TEXT NOT NULL DEFAULT '',
	metadata           JSONB NOT NULL DEFAULT '{}'::jsonb,
	embedding          vector(${EMBEDDING_DIM}),
	CONSTRAINT memory_facts_single_open_interval EXCLUDE USING gist (
		scope_key WITH =, subject_key WITH =, attribute_key WITH =, valid WITH &&
	) WHERE (cardinality = 'single' AND retracted_at IS NULL AND corroborates IS NULL)
);

CREATE INDEX IF NOT EXISTS idx_memory_facts_source_chunk ON memory_facts (source_chunk_id);
CREATE INDEX IF NOT EXISTS idx_memory_facts_source_document ON memory_facts (source_document_id);
CREATE INDEX IF NOT EXISTS idx_memory_facts_chain ON memory_facts (scope_key, subject_key, attribute_key);
CREATE INDEX IF NOT EXISTS idx_memory_facts_tags ON memory_facts USING gin (metadata jsonb_path_ops);
CREATE INDEX IF NOT EXISTS idx_memory_facts_embedding ON memory_facts USING hnsw (embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64);

-- One row per (document, content) the extractor has run over: a re-ingest of the same
-- content spends no second LLM call. content_hash digests the document's chunk ids, which
-- are themselves content-derived.
CREATE TABLE IF NOT EXISTS memory_fact_extractions (
	document_id   TEXT NOT NULL,
	content_hash  TEXT NOT NULL,
	facts         INTEGER NOT NULL DEFAULT 0,
	extracted_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (document_id, content_hash)
);
