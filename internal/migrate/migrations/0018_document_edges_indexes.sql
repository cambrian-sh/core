-- 0018_document_edges_indexes.sql — the two lookup indexes document_edges always needed
-- (ADR-0064 runner; ADR-0017 spreading activation;
-- T-20261031-document-edges-has-no-source-id-target-id-index).
--
-- The spreading engine and KG expansion walk edges by source_id and by target_id. The
-- baseline (0001) gave document_edges only its PRIMARY KEY (source_id, target_id,
-- edge_type), which serves the source_id lookup as a prefix scan but nothing serves the
-- target_id side — so every reverse hop was a sequential scan of the whole edge table.
-- The legacy db/migrations/006_document_edges_update.sql (deleted 2026-10-31) had created
-- both indexes, which is why stores provisioned from that directory never showed the
-- cost and stores provisioned by this runner did.
--
-- IDEMPOTENT. IF NOT EXISTS on both, so a store that still carries the legacy indexes
-- (same names) is a no-op, and so is a replay.

CREATE INDEX IF NOT EXISTS idx_doc_edges_source ON document_edges (source_id);
CREATE INDEX IF NOT EXISTS idx_doc_edges_target ON document_edges (target_id);
