-- 0001_baseline.sql — the head schema baseline (PLAT-02 / ADR-0064).
-- GENERATED from postgres.BaselineStatements; ${EMBEDDING_DIM} is substituted
-- from config (embedder.dimensions) by the migration runner at apply time.
-- pgvector cannot ALTER a VECTOR column's dimension, so it is baked at CREATE.

CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE IF NOT EXISTS documents (
			id TEXT PRIMARY KEY,
			text TEXT NOT NULL,
			embedding VECTOR(${EMBEDDING_DIM}),
			metadata JSONB,
			access_count INT DEFAULT 0,
			activation_strength DOUBLE PRECISION NOT NULL DEFAULT 0.1,
			scoring_prompt_version VARCHAR(8) NOT NULL DEFAULT '',
			last_accessed_at TIMESTAMP,
			created_at TIMESTAMP NOT NULL DEFAULT NOW(),
			document_type VARCHAR(32) NOT NULL DEFAULT 'memory',
			version INT DEFAULT 1,
			summary TEXT NOT NULL DEFAULT ''
		);

ALTER TABLE documents ADD COLUMN IF NOT EXISTS version INT DEFAULT 1;

ALTER TABLE documents ADD COLUMN IF NOT EXISTS summary TEXT NOT NULL DEFAULT '';

ALTER TABLE documents ADD COLUMN IF NOT EXISTS activation_strength DOUBLE PRECISION NOT NULL DEFAULT 0.1;

ALTER TABLE documents ADD COLUMN IF NOT EXISTS scoring_prompt_version VARCHAR(8) NOT NULL DEFAULT '';

ALTER TABLE documents ADD COLUMN IF NOT EXISTS created_at TIMESTAMP NOT NULL DEFAULT NOW();

ALTER TABLE documents DROP COLUMN IF EXISTS importance_score;

DROP INDEX IF EXISTS idx_doc_embedding;

CREATE OR REPLACE FUNCTION update_activation_strength(doc_id TEXT, delta DOUBLE PRECISION)
			RETURNS VOID AS $$
			BEGIN
				UPDATE documents
				SET activation_strength = LEAST(0.8, GREATEST(0.0, activation_strength + delta))
				WHERE id = doc_id;
			END;
			$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION apply_ebbinghaus_decay(min_gc_age_days INT DEFAULT 30)
			RETURNS VOID AS $$
			DECLARE
				lambda  CONSTANT DOUBLE PRECISION := 0.001;
				epsilon CONSTANT DOUBLE PRECISION := 0.02;
				eta     CONSTANT DOUBLE PRECISION := 0.005;
			BEGIN
				UPDATE documents
				SET activation_strength = GREATEST(0.0, LEAST(1.0,
					(activation_strength + eta * access_count) * EXP(-1 * lambda) * (1 - epsilon)
				));

				DELETE FROM documents
				WHERE activation_strength <= 0.05
				  AND access_count = 0
				  AND created_at < NOW() - (min_gc_age_days || ' days')::INTERVAL;
			END;
			$$ LANGUAGE plpgsql;

CREATE INDEX IF NOT EXISTS idx_doc_metadata ON documents USING gin (metadata jsonb_path_ops);

CREATE INDEX IF NOT EXISTS idx_doc_embedding_cosine ON documents USING hnsw (embedding vector_cosine_ops) WITH (m = 24, ef_construction = 100);

CREATE INDEX IF NOT EXISTS idx_doc_fts ON documents USING gin (to_tsvector('english', text));

CREATE TABLE IF NOT EXISTS document_edges (
			source_id TEXT NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
			target_id TEXT NOT NULL,
			edge_type VARCHAR(50) NOT NULL,
			label TEXT,
			weight REAL NOT NULL DEFAULT 0.5,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (source_id, target_id, edge_type)
		);

ALTER TABLE document_edges DROP CONSTRAINT IF EXISTS document_edges_target_id_fkey;

ALTER TABLE document_edges ADD COLUMN IF NOT EXISTS label TEXT;

ALTER TABLE document_edges ADD COLUMN IF NOT EXISTS edge_type VARCHAR(50) NOT NULL DEFAULT '';

ALTER TABLE document_edges ADD COLUMN IF NOT EXISTS weight REAL NOT NULL DEFAULT 0.5;

ALTER TABLE document_edges ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ NOT NULL DEFAULT NOW();

UPDATE documents SET created_at = NOW() WHERE created_at < '1900-01-01';

UPDATE document_edges SET created_at = NOW() WHERE created_at < '1900-01-01';

CREATE TABLE IF NOT EXISTS chunk_triplets (
			chunk_id  TEXT NOT NULL,
			h         TEXT NOT NULL,
			r         TEXT NOT NULL,
			t         TEXT NOT NULL,
			weight    REAL NOT NULL DEFAULT 1.0,
			extracted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (chunk_id, h, r, t)
		);

CREATE EXTENSION IF NOT EXISTS ltree;

ALTER TABLE documents ADD COLUMN IF NOT EXISTS section_path TEXT NOT NULL DEFAULT '';

ALTER TABLE documents ADD COLUMN IF NOT EXISTS parent_section_id TEXT NOT NULL DEFAULT '';

ALTER TABLE documents ADD COLUMN IF NOT EXISTS section_ltree LTREE;

CREATE INDEX IF NOT EXISTS idx_doc_section_ltree ON documents USING GIST (section_ltree);

CREATE INDEX IF NOT EXISTS idx_doc_parent_section ON documents (parent_section_id);

CREATE INDEX IF NOT EXISTS idx_chunk_triplets_h ON chunk_triplets (h);

CREATE INDEX IF NOT EXISTS idx_chunk_triplets_t ON chunk_triplets (t);

ALTER TABLE chunk_triplets ADD COLUMN IF NOT EXISTS confidence SMALLINT;

ALTER TABLE chunk_triplets ADD COLUMN IF NOT EXISTS sources TEXT[];

CREATE INDEX IF NOT EXISTS idx_chunk_triplets_sources ON chunk_triplets USING GIN (sources);

CREATE TABLE IF NOT EXISTS chunk_pagerank (
			chunk_id    TEXT PRIMARY KEY,
			score       REAL NOT NULL DEFAULT 0,
			computed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);

CREATE TABLE IF NOT EXISTS chunk_pagerank_meta (
			id            INT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
			computed_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			chunk_count   INT NOT NULL DEFAULT 0,
			triplet_count INT NOT NULL DEFAULT 0
		);

