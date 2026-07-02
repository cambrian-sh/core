-- Migration 005: Ebbinghaus decay stored procedure with pg_cron scheduling.
--
-- ADR-0015 (Engram Engine §4): apply_ebbinghaus_decay runs nightly at 03:00 UTC.
-- Formula per document: AS_new = (AS_current + η·access_count) · e^{-λ} · (1-ε)
--   λ = 0.001  (half-life ≈ 693 days)
--   ε = 0.02   (interference damping)
--   η = 0.005  (retrieval bonus coefficient)
-- Result clamped to [0.0, 1.0].
--
-- GC: delete documents where activation_strength ≤ 0.05 AND access_count = 0
--      AND created_at < NOW() - INTERVAL 'min_gc_age_days days' (default 30).
--
-- PREREQUISITE: pg_cron extension must be installed:
--   CREATE EXTENSION IF NOT EXISTS pg_cron;
--   ALTER EXTENSION pg_cron UPDATE;
--
-- Idempotent: safe to run twice (OR REPLACE prevents errors on re-run).

-- Add created_at column if not present (idempotent).
ALTER TABLE documents ADD COLUMN IF NOT EXISTS created_at TIMESTAMP NOT NULL DEFAULT NOW();

-- Create or replace the decay stored procedure.
CREATE OR REPLACE FUNCTION apply_ebbinghaus_decay(min_gc_age_days INT DEFAULT 30)
RETURNS VOID AS $$
DECLARE
    lambda CONSTANT DOUBLE PRECISION := 0.001;
    epsilon CONSTANT DOUBLE PRECISION := 0.02;
    eta CONSTANT DOUBLE PRECISION := 0.005;
BEGIN
    -- Apply Ebbinghaus decay to all documents.
    UPDATE documents
    SET activation_strength = GREATEST(0.0, LEAST(1.0,
        (activation_strength + eta * access_count) * EXP(-1 * lambda) * (1 - epsilon)
    ));

    -- GC: remove stale, unaccessed documents past the minimum age gate.
    DELETE FROM documents
    WHERE activation_strength <= 0.05
      AND access_count = 0
      AND created_at < NOW() - (min_gc_age_days || ' days')::INTERVAL;
END;
$$ LANGUAGE plpgsql;

-- Schedule nightly decay at 03:00 UTC via pg_cron.
-- IMPORTANT: pg_cron must be installed and running. If pg_cron is unavailable,
--            the CircadianRhythm health probe will log a startup warning.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_extension WHERE extname = 'pg_cron'
    ) THEN
        PERFORM cron.schedule(
            'engram-ebbinghaus-decay',
            '0 3 * * *',
            'SELECT apply_ebbinghaus_decay(30);'
        );
    END IF;
END;
$$ LANGUAGE plpgsql;
