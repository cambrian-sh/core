-- Migration 004: HNSW cosine index rebuild + retrieve_and_update_memories stored procedure.
--
-- ADR-0015 (Engram Engine §5): Switch from L2 distance (<->) to cosine distance (<=>) for all retrieval.
-- The HNSW index must use vector_cosine_ops for geometric correctness.
--
-- Also creates the update_activation_strength stored procedure that bumps activation_strength
-- by +0.05 per retrieval, clamped to [0.0, 0.8] (maturation ceiling).
--
-- Idempotent: safe to run twice (IF EXISTS / OR REPLACE prevents errors on re-run).

-- Drop any old L2-based index.
DROP INDEX IF EXISTS idx_doc_embedding;

-- Create cosine-based HNSW index if not already present.
CREATE INDEX IF NOT EXISTS idx_doc_embedding_cosine ON documents USING hnsw (embedding vector_cosine_ops) WITH (m = 24, ef_construction = 100);

-- Stored procedure for bumping activation_strength on retrieval.
CREATE OR REPLACE FUNCTION update_activation_strength(doc_id TEXT, delta DOUBLE PRECISION)
RETURNS VOID AS $$
BEGIN
    UPDATE documents
    SET activation_strength = LEAST(0.8, GREATEST(0.0, activation_strength + delta))
    WHERE id = doc_id;
END;
$$ LANGUAGE plpgsql;
