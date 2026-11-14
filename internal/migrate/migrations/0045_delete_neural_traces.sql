-- 0045_delete_neural_traces.sql — delete the stored agent neural traces (owner
-- decision 2026-11-14, cambrian-knowledge/decisions/owner-decisions-2026-11-14-findings-round.md;
-- ENG-1462, T-20261113-agent-neural-traces-full-prompt-response-are).
--
-- Neural traces were deleted completely: "not memory, not telemetry". The writer
-- (network.storeNeuralTrace — the full PROMPT + RESPONSE of every agent model stream,
-- and each plan step's _thought_trace) wrote them through the raw tier-0 store,
-- unclassified and without an owner tag, so a trace copied classified context into a
-- row any reader without Required/AnyOf terms could reach through the lexical lane,
-- GetByID or QueryByMetadata. The writer, the raw store hand-off to the network server
-- and the only reader (a type skip in memory.Agent.FetchContext) were removed in the same
-- change. With the skip gone, a surviving row would surface in that context, so the rows
-- go too.
--
-- WHAT IS DELETED, in dependency order:
--   1. document_edges rows whose source or target is a trace (no FK cascades into
--      document_edges, 0009). Traces were never linked, so this is expected to be empty;
--      it is here so a hand-made edge cannot dangle.
--   2. chunk_embeddings rows of trace chunks. Traces carried no embedding, so also
--      expected empty; deleted explicitly rather than relying on the 0013 cascade.
--   3. chunks rows with document_type = 'neural_trace' (tableFor sends an unregistered
--      type to chunks, so that is the only table that held them).
--
-- DELIBERATELY NOT TOUCHED: documents_legacy (0007's frozen pre-split copy, read by
-- nothing).
--
-- DESTRUCTIVE and forward-only, through the boot-time migration policy like every other
-- file (database.migrations.mode, protected_dbnames, ADR-0064). Nothing of evidential
-- value is lost: nothing ever read a trace.
--
-- IDEMPOTENT: every statement is a DELETE filtered on the rows this file removes.

DELETE FROM document_edges e
 WHERE EXISTS (SELECT 1 FROM chunks c
                WHERE c.document_type = 'neural_trace'
                  AND (c.id = e.source_id OR c.id = e.target_id));

DELETE FROM chunk_embeddings ce
 USING chunks c
 WHERE ce.chunk_id = c.id
   AND c.document_type = 'neural_trace';

DELETE FROM chunks WHERE document_type = 'neural_trace';
