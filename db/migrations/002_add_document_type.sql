-- Migration 002: Add document_type discriminator column to documents table.
--
-- This allows Agent Profiles (Gatekeeper Issue #011) and Judicial Records
-- (Verifier Pool Issue #013) to coexist with memory documents in the same
-- pgvector table without polluting memory retrieval queries.
--
-- Idempotent: safe to run twice (IF NOT EXISTS prevents duplicate column errors).

ALTER TABLE documents ADD COLUMN IF NOT EXISTS document_type VARCHAR(32) NOT NULL DEFAULT 'memory';

-- Backfill: any rows that slipped through with an empty value are set to 'memory'.
UPDATE documents SET document_type = 'memory' WHERE document_type IS NULL OR document_type = '';
