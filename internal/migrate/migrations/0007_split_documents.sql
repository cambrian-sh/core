-- 0007_split_documents.sql — one table stops meaning six things (ADR-0093).
--
-- `documents` held everything with an embedding: document chunks, agent-written memories,
-- scenes, entities, and the seeded tool/skill/agent_profile descriptors that are rebuilt on
-- every boot. Three consequences, in ascending order of seriousness:
--
--   1. One HNSW index served all of them, so a recall search traversed tool and skill
--      descriptors that recall can never return.
--   2. `document_type` had no index despite every read filtering on it.
--   3. A DOCUMENT WAS NOT AN ENTITY. Nothing in the database represented the source
--      document a chunk came from — parentage lived in an id string convention
--      (`{docID}-chunk-{n}`) and a metadata key. So classification tags were COPIED onto
--      every chunk at ingest, with no authoritative row to copy from.
--
-- (3) is why this migration exists. Re-tagging a document meant updating N chunk rows with
-- no transaction boundary; a partial failure left a document half-classified, some chunks
-- reachable and some not. A boundary that can be half-applied is not a boundary. After this,
-- `documents.tags` is authoritative and the per-chunk copy is an explicitly derived cache.
--
-- SAFETY: this migration is ADDITIVE. The old table is RENAMED, never dropped, and every row
-- is copied rather than moved. Rollback is `DROP` the new tables and rename back. The corpus
-- is the shared store the benchmarks measure against, so it is never destroyed to make a
-- schema tidy.
--
-- Append-only per ADR-0064; idempotent.

-- ── Step 1: preserve the old table under a name that says what it is ────────────────────
-- Guarded so a re-run is a no-op rather than an error. The rename carries the existing
-- indexes and data with it; nothing is rebuilt and nothing is lost.
DO $$
BEGIN
	IF EXISTS (SELECT 1 FROM information_schema.tables
	           WHERE table_schema = current_schema() AND table_name = 'documents')
	   AND NOT EXISTS (SELECT 1 FROM information_schema.tables
	                   WHERE table_schema = current_schema() AND table_name = 'documents_legacy')
	THEN
		ALTER TABLE documents RENAME TO documents_legacy;
	END IF;
END $$;

-- ── Step 2: the document, as an actual entity ───────────────────────────────────────────
-- Deliberately has NO embedding. A full document is not a retrieval unit — chunks are — and
-- giving it a vector would put it back in the recall index this migration exists to clean.
--
-- `tags` is a real column rather than a metadata key because it is now load-bearing for
-- access control, and a classification you cannot constrain, index, or see in the schema is
-- one that drifts.
CREATE TABLE IF NOT EXISTS documents (
	id            TEXT PRIMARY KEY,
	title         TEXT NOT NULL DEFAULT '',
	source_type   TEXT NOT NULL DEFAULT '',
	text          TEXT NOT NULL DEFAULT '',
	tags          TEXT[] NOT NULL DEFAULT '{}',
	metadata      JSONB,
	created_at    TIMESTAMP NOT NULL DEFAULT NOW(),
	updated_at    TIMESTAMP NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_documents_tags ON documents USING GIN (tags);

-- ── Step 3: chunks — every vectorised unit recall can return ────────────────────────────
-- All the mnemonic_* types, episodic memory, and the older judicial/procedural/neural/
-- negative types live here TOGETHER, because they always did: recall searched one table and
-- still does, so this migration does not change what a search can find. Splitting them
-- further would have quietly narrowed recall, which the benchmarks would have caught and
-- nobody would have wanted.
--
-- `document_id` is NULLABLE on purpose. A chunk carved out of an ingested document has a
-- parent; a memory an agent wrote about what it just did does not. Forcing the second kind
-- under a synthetic document would be a lie told to satisfy a foreign key.
CREATE TABLE IF NOT EXISTS chunks (
	id                     TEXT PRIMARY KEY,
	document_id            TEXT REFERENCES documents(id) ON DELETE CASCADE,
	chunk_index            INT,
	text                   TEXT NOT NULL,
	embedding              VECTOR(${EMBEDDING_DIM}),
	metadata               JSONB,
	access_count           INT DEFAULT 0,
	activation_strength    DOUBLE PRECISION NOT NULL DEFAULT 0.1,
	scoring_prompt_version VARCHAR(8) NOT NULL DEFAULT '',
	last_accessed_at       TIMESTAMP,
	created_at             TIMESTAMP NOT NULL DEFAULT NOW(),
	document_type          VARCHAR(32) NOT NULL DEFAULT 'mnemonic_fact',
	version                INT DEFAULT 1,
	summary                TEXT NOT NULL DEFAULT '',
	section_path           TEXT NOT NULL DEFAULT '',
	parent_section_id      TEXT NOT NULL DEFAULT '',
	section_ltree          LTREE
);

-- The indexes the old table had, now serving only rows recall can actually return.
CREATE INDEX IF NOT EXISTS idx_chunks_metadata ON chunks USING GIN (metadata jsonb_path_ops);
CREATE INDEX IF NOT EXISTS idx_chunks_embedding_cosine ON chunks USING hnsw (embedding vector_cosine_ops) WITH (m = 24, ef_construction = 100);
CREATE INDEX IF NOT EXISTS idx_chunks_fts ON chunks USING GIN (to_tsvector('english', text));
CREATE INDEX IF NOT EXISTS idx_chunks_section_ltree ON chunks USING GIST (section_ltree);
CREATE INDEX IF NOT EXISTS idx_chunks_parent_section ON chunks (parent_section_id);
-- The index whose absence made every typed read a sequential scan.
CREATE INDEX IF NOT EXISTS idx_chunks_document_type ON chunks (document_type);
CREATE INDEX IF NOT EXISTS idx_chunks_document_id ON chunks (document_id);

-- ── Step 4: structural section nodes ────────────────────────────────────────────────────
-- ADR-0060 nodes are not embedded and are excluded from fact recall by design, so they were
-- pure ballast in a table whose reason for existing is a vector index.
CREATE TABLE IF NOT EXISTS document_sections (
	id                TEXT PRIMARY KEY,
	document_id       TEXT REFERENCES documents(id) ON DELETE CASCADE,
	title             TEXT NOT NULL DEFAULT '',
	metadata          JSONB,
	section_path      TEXT NOT NULL DEFAULT '',
	parent_section_id TEXT NOT NULL DEFAULT '',
	section_ltree     LTREE,
	created_at        TIMESTAMP NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_sections_ltree ON document_sections USING GIST (section_ltree);
CREATE INDEX IF NOT EXISTS idx_sections_parent ON document_sections (parent_section_id);
CREATE INDEX IF NOT EXISTS idx_sections_document_id ON document_sections (document_id);

-- ── Step 5: the seeded descriptors ──────────────────────────────────────────────────────
-- Tools, skills and agent profiles are CONFIGURATION that happens to be searched
-- semantically. They are recreated on every boot, they are never recall results, and their
-- presence in the recall table is what forced the `document_type NOT IN (...)` guards that
-- exist in the adapter today. Each gets its own small HNSW index, which is also a faster one.
-- They deliberately keep the SAME column set as chunks (minus document parentage). The
-- temptation was to trim them to id/text/embedding/metadata, and it was the wrong call: the
-- adapter upserts and reads one row shape, so three narrower tables would have bought tidier
-- DDL at the price of three code paths through the write path. The separation that matters
-- here is the INDEX, not the columns.
CREATE TABLE IF NOT EXISTS tools (
	id                     TEXT PRIMARY KEY,
	text                   TEXT NOT NULL,
	embedding              VECTOR(${EMBEDDING_DIM}),
	metadata               JSONB,
	access_count           INT DEFAULT 0,
	activation_strength    DOUBLE PRECISION NOT NULL DEFAULT 0.1,
	scoring_prompt_version VARCHAR(8) NOT NULL DEFAULT '',
	last_accessed_at       TIMESTAMP,
	created_at             TIMESTAMP NOT NULL DEFAULT NOW(),
	document_type          VARCHAR(32) NOT NULL DEFAULT 'tool',
	version                INT DEFAULT 1,
	summary                TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS skills (
	id                     TEXT PRIMARY KEY,
	text                   TEXT NOT NULL,
	embedding              VECTOR(${EMBEDDING_DIM}),
	metadata               JSONB,
	access_count           INT DEFAULT 0,
	activation_strength    DOUBLE PRECISION NOT NULL DEFAULT 0.1,
	scoring_prompt_version VARCHAR(8) NOT NULL DEFAULT '',
	last_accessed_at       TIMESTAMP,
	created_at             TIMESTAMP NOT NULL DEFAULT NOW(),
	document_type          VARCHAR(32) NOT NULL DEFAULT 'skill',
	version                INT DEFAULT 1,
	summary                TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS agent_profiles (
	id                     TEXT PRIMARY KEY,
	text                   TEXT NOT NULL,
	embedding              VECTOR(${EMBEDDING_DIM}),
	metadata               JSONB,
	access_count           INT DEFAULT 0,
	activation_strength    DOUBLE PRECISION NOT NULL DEFAULT 0.1,
	scoring_prompt_version VARCHAR(8) NOT NULL DEFAULT '',
	last_accessed_at       TIMESTAMP,
	created_at             TIMESTAMP NOT NULL DEFAULT NOW(),
	document_type          VARCHAR(32) NOT NULL DEFAULT 'agent_profile',
	version                INT DEFAULT 1,
	summary                TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_tools_embedding ON tools USING hnsw (embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64);
CREATE INDEX IF NOT EXISTS idx_skills_embedding ON skills USING hnsw (embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64);
CREATE INDEX IF NOT EXISTS idx_agent_profiles_embedding ON agent_profiles USING hnsw (embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64);
CREATE INDEX IF NOT EXISTS idx_tools_metadata ON tools USING GIN (metadata jsonb_path_ops);
CREATE INDEX IF NOT EXISTS idx_skills_metadata ON skills USING GIN (metadata jsonb_path_ops);
CREATE INDEX IF NOT EXISTS idx_agent_profiles_metadata ON agent_profiles USING GIN (metadata jsonb_path_ops);

-- ── Step 6: backfill the documents entity from the parentage that was only ever a string ─
-- Chunks recorded their parent as `metadata->>'document_id'`. That key is the only record
-- that a source document ever existed, so it is what the entity is reconstructed from.
-- Title and source type are recovered from chunk metadata where they were carried.
--
-- Tags are lifted from the chunks' own copies: they all descend from one document's tags, so
-- any chunk carrying them is a faithful source. DISTINCT collapses the N duplicates back into
-- the single authoritative row this schema should have had from the start.
INSERT INTO documents (id, title, source_type, tags, metadata, created_at)
SELECT
	d.doc_id,
	COALESCE(MAX(d.metadata->>'title'), ''),
	COALESCE(MAX(d.metadata->>'source_type'), ''),
	COALESCE(
		(ARRAY(SELECT DISTINCT jsonb_array_elements_text(
			(SELECT l2.metadata->'tags' FROM documents_legacy l2
			 WHERE l2.metadata->>'document_id' = d.doc_id
			   AND jsonb_typeof(l2.metadata->'tags') = 'array'
			 LIMIT 1)
		))),
		'{}'
	),
	jsonb_build_object('backfilled_from', 'documents_legacy'),
	MIN(d.created_at)
FROM (
	SELECT metadata->>'document_id' AS doc_id, metadata, created_at
	FROM documents_legacy
	WHERE metadata->>'document_id' IS NOT NULL
	  AND metadata->>'document_id' <> ''
) d
GROUP BY d.doc_id
ON CONFLICT (id) DO NOTHING;

-- ── Step 7: copy every row into the table that now owns it ──────────────────────────────
-- Chunks: everything recall can return. The parent link is set only where the reconstructed
-- document actually exists, so an orphan chunk keeps a NULL rather than a dangling id.
INSERT INTO chunks (
	id, document_id, text, embedding, metadata, access_count, activation_strength,
	scoring_prompt_version, last_accessed_at, created_at, document_type, version, summary,
	section_path, parent_section_id, section_ltree
)
SELECT
	l.id,
	doc.id,
	l.text, l.embedding, l.metadata, l.access_count, l.activation_strength,
	l.scoring_prompt_version, l.last_accessed_at, l.created_at, l.document_type, l.version,
	l.summary, l.section_path, l.parent_section_id, l.section_ltree
FROM documents_legacy l
LEFT JOIN documents doc ON doc.id = l.metadata->>'document_id'
WHERE l.document_type NOT IN ('tool', 'skill', 'agent_profile', 'doc_section')
ON CONFLICT (id) DO NOTHING;

INSERT INTO document_sections (id, document_id, title, metadata, section_path, parent_section_id, section_ltree, created_at)
SELECT l.id, doc.id, l.text, l.metadata, l.section_path, l.parent_section_id, l.section_ltree, l.created_at
FROM documents_legacy l
LEFT JOIN documents doc ON doc.id = l.metadata->>'document_id'
WHERE l.document_type = 'doc_section'
ON CONFLICT (id) DO NOTHING;

INSERT INTO tools (id, text, embedding, metadata, access_count, activation_strength,
	scoring_prompt_version, last_accessed_at, created_at, document_type, version, summary)
SELECT id, text, embedding, metadata, access_count, activation_strength,
	scoring_prompt_version, last_accessed_at, created_at, document_type, version, summary
FROM documents_legacy WHERE document_type = 'tool'
ON CONFLICT (id) DO NOTHING;

INSERT INTO skills (id, text, embedding, metadata, access_count, activation_strength,
	scoring_prompt_version, last_accessed_at, created_at, document_type, version, summary)
SELECT id, text, embedding, metadata, access_count, activation_strength,
	scoring_prompt_version, last_accessed_at, created_at, document_type, version, summary
FROM documents_legacy WHERE document_type = 'skill'
ON CONFLICT (id) DO NOTHING;

INSERT INTO agent_profiles (id, text, embedding, metadata, access_count, activation_strength,
	scoring_prompt_version, last_accessed_at, created_at, document_type, version, summary)
SELECT id, text, embedding, metadata, access_count, activation_strength,
	scoring_prompt_version, last_accessed_at, created_at, document_type, version, summary
FROM documents_legacy WHERE document_type = 'agent_profile'
ON CONFLICT (id) DO NOTHING;

-- ── Step 8: repoint the stored functions ────────────────────────────────────────────────
-- THE TRAP THIS MIGRATION ALMOST WALKED INTO. Both functions name `documents` in their body,
-- and plpgsql resolves that name at CALL time. Renaming the table without redefining them
-- would have left activation updates and Ebbinghaus decay silently operating on the new,
-- nearly-empty document table: every call succeeding, nothing being decayed, no error
-- anywhere. Same failure shape as every other bug this subsystem has produced — quiet, and
-- reported as success.
CREATE OR REPLACE FUNCTION update_activation_strength(doc_id TEXT, delta DOUBLE PRECISION)
RETURNS VOID AS $$
BEGIN
	UPDATE chunks
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
	UPDATE chunks
	SET activation_strength = GREATEST(0.0, LEAST(1.0,
		(activation_strength + eta * access_count) * EXP(-1 * lambda) * (1 - epsilon)
	));

	DELETE FROM chunks
	WHERE activation_strength <= 0.05
	  AND access_count = 0
	  AND created_at < NOW() - (min_gc_age_days || ' days')::INTERVAL;
END;
$$ LANGUAGE plpgsql;
