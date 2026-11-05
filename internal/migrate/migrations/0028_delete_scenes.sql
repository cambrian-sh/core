-- 0028_delete_scenes.sql — delete the stored scenes (owner decision 2026-11-05,
-- cambrian-knowledge/decisions/scenes-cancelled.md;
-- T-20261105-remove-scenes-completely-owner-decision-2026-11).
--
-- Scenes were cancelled completely. The code that wrote them (the ADR-0028 ingest
-- SceneGenerator, the ADR-0025 per-step scene, the ADR-0049 plan scene) and every reader
-- (the precedent lane, the "scenes" x-lane, WorkspaceStage scene priming, ADR-0094
-- induction) were deleted in the same change. What remains in a database is inert rows
-- that nothing reads, and one of them would still compete in a system-scope search that
-- does not filter by type. They go.
--
-- WHAT IS DELETED, in dependency order:
--   1. document_edges rows of the scene-only types `discussed_in` (a step fact → its
--      scene, ADR-0025) and `engaged` (a scene → an entity, ADR-0049; declared, never
--      written), and every edge whose source or target is a scene row. document_edges
--      has NO foreign key on either endpoint (0009 dropped the last one), so nothing
--      cascades into it: an edge to a deleted scene would otherwise dangle forever and the
--      spreading engine would keep fetching a target that cannot exist.
--   2. chunk_embeddings rows of scene chunks (ADR-0107 projections). They also cascade
--      (`REFERENCES chunks(id) ON DELETE CASCADE`, 0013); deleted explicitly so the file
--      states what it removes rather than relying on the reader knowing the FK.
--   3. chunks rows with document_type = 'mnemonic_scene'. experience_derivations rows
--      that point at one cascade with it (0010); the `experiences` parent rows stay —
--      they are the ADR-0095 episodes, which are still written and closed.
--
-- DELIBERATELY NOT TOUCHED:
--   - documents_legacy (0007's frozen copy of the pre-split table). Nothing reads it, and
--     it is the record of what the split moved; rewriting it is not this file's business.
--   - `follows` and `closes` edges. Their writers (the deleted raw step-record path) are
--     gone too, but their endpoints are ordinary mnemonic_fact rows that still exist, so
--     the edges are not dangling; the owner's scope was scenes.
--   - the scoring_prompt_version column (written by the deleted tier-2 judge). Dropping a
--     column is a separate decision (investigations/2026-11-05-memory-judge-assessment.md).
--
-- DESTRUCTIVE and forward-only: the rows are not recoverable from the database once this
-- runs. It goes through the boot-time migration policy like every other file
-- (database.migrations.mode, protected_dbnames, ADR-0064): a protected database refuses
-- it at boot until the operator opts in with allow_pending or runs `migrate up`. No scene
-- row was ever measured (ADR-0049, ADR-0094 `measurements: []`), so nothing of evidential
-- value is lost.
--
-- IDEMPOTENT: every statement is a DELETE filtered on rows that this file removes, so a
-- replay, a fresh database and an OSS database that never wrote a scene are all no-ops.

DELETE FROM document_edges e
 WHERE e.edge_type IN ('discussed_in', 'engaged')
    OR EXISTS (SELECT 1 FROM chunks c
                WHERE c.document_type = 'mnemonic_scene'
                  AND (c.id = e.source_id OR c.id = e.target_id));

DELETE FROM chunk_embeddings ce
 USING chunks c
 WHERE ce.chunk_id = c.id
   AND c.document_type = 'mnemonic_scene';

DELETE FROM chunks WHERE document_type = 'mnemonic_scene';
