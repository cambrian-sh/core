-- Migration 006: Update document_edges table for ADR-0017 spreading activation.
-- Adds edge_type, weight, created_at columns and composite primary key.
-- Idempotent: safe to run twice.

-- Drop old PK if it exists (old schema had (source_id, target_id) only).
ALTER TABLE document_edges DROP CONSTRAINT IF EXISTS document_edges_pkey;

-- Add new columns (idempotent).
ALTER TABLE document_edges ADD COLUMN IF NOT EXISTS edge_type VARCHAR(50) NOT NULL DEFAULT '';
ALTER TABLE document_edges ADD COLUMN IF NOT EXISTS weight REAL NOT NULL DEFAULT 0.5;
ALTER TABLE document_edges ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ NOT NULL DEFAULT NOW();

-- Add new composite primary key (idempotent — safe if already exists).
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'document_edges_pkey' AND conrelid = 'document_edges'::regclass
    ) THEN
        ALTER TABLE document_edges ADD PRIMARY KEY (source_id, target_id, edge_type);
    END IF;
END;
$$;

-- Create indexes (idempotent).
CREATE INDEX IF NOT EXISTS idx_doc_edges_source ON document_edges(source_id);
CREATE INDEX IF NOT EXISTS idx_doc_edges_target ON document_edges(target_id);
