-- Migration 010: chunk_pagerank — the PageRank structural prior (ADR-0054 D2).
--
-- PageRank over the shared-entity chunk graph (built from chunk_triplets) is a
-- query-UNAWARE structural importance score: a chunk that bridges many entities
-- scores high. It is ONE signal in the Stage-A multi-signal blend (weight ~0.05).
--
-- Producer/consumer split (edge-device friendly): the kernel only READS this
-- table. It is POPULATED by the always-up recompute worker (cmd/pagerank-recompute)
-- — a separate container so the schedule does not depend on the (intermittent)
-- kernel being up. The table is producer-agnostic: a pg_cron stored proc could
-- populate it instead with zero kernel change.
--
--   score       — PageRank in [0,1], a probability distribution over chunks.
--   computed_at — when this row's score was written (the worker stamps the batch).
--
-- chunk_id is TEXT (= documents.id), no FK: an orphaned score is harmless (the
-- blend just won't find a chunk that no longer exists) and a strict FK would
-- force CASCADE coupling we don't want for a derived signal.
--
-- chunk_pagerank_meta is a single-row table the worker uses to decide whether a
-- recompute is needed (corpus delta) and to expose freshness. Idempotent.

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
