-- 0064_chunks_text_tsv.sql — a stored keyword column for the lexical lane
-- (T-20261031-lexical-lane-or-joins-every-query-term-and, ENG-111; owner decision
-- 2026-11-19, Part A).
-- cambrian:not-destructive: adds a generated column (table rewrite)
--
-- The lexical lane matched with the expression index idx_chunks_fts but RANKED with
-- ts_rank_cd(to_tsvector('english', text), …), which re-parses every matched row's text
-- on every query. text_tsv stores exactly that expression, so LexicalSearch matches and
-- ranks from the column and the ranking is unchanged (the differential test in
-- internal/infrastructure/postgres proves the same order and scores).
--
-- Adding a STORED generated column rewrites every chunk row once (an ACCESS EXCLUSIVE
-- lock for the rewrite). The 'english' configuration stays (ENG-898: no Turkish
-- retrieval yet); a later language change will need another rewrite, accepted.
--
-- Rollback:
--   CREATE INDEX IF NOT EXISTS idx_chunks_fts ON chunks USING GIN (to_tsvector('english', text));
--   DROP INDEX IF EXISTS idx_chunks_text_tsv;
--   ALTER TABLE chunks DROP COLUMN IF EXISTS text_tsv;

ALTER TABLE chunks ADD COLUMN IF NOT EXISTS text_tsv tsvector
    GENERATED ALWAYS AS (to_tsvector('english', text)) STORED;

CREATE INDEX IF NOT EXISTS idx_chunks_text_tsv ON chunks USING GIN (text_tsv);

-- The expression index served the same match; the column's index replaces it.
DROP INDEX IF EXISTS idx_chunks_fts;
