-- ENG-1068 (owner decision 2026-11-14): substrate-lane typed rows are keyed
-- `<stream>:<source_ref>`. Before the fix, the premium substrate lane (evidence
-- source_id 'event:<stream>') keyed its events and observations on the caller's
-- source_ref alone, which is unique only within a stream, so stream A's '42' and
-- stream B's '42' collided on (namespace_id, source_ref). This one-off migration
-- re-keys the rows written before the fix to the bare `<stream>:<source_ref>` form
-- the lane now uses for revision-less evidence; an observation keeps its '#obs<i>'
-- suffix (it is part of the old ref). Rows that collided before the fix were
-- dropped at write time and cannot be recovered here; re-sending those envelopes
-- writes them now (as a new revision).
--
-- Which rows: those whose evidence is a substrate-lane row with NO revision (every
-- pre-fix delivery; a post-fix delivery always carries one and is keyed
-- `<stream>:<source_ref>#<revision>` already), and whose ref is not already
-- `<stream>:`-prefixed (a revision-less row the fixed lane re-transformed). So a
-- re-run, or a deployment where the fixed lane already wrote, changes nothing.
--
-- How: the new keys are staged first. A key that would collide with a row this
-- migration does not rewrite (or with another staged key) is left unchanged rather
-- than failing the boot. The rewrite is then done in two passes through a sentinel
-- prefix (chr(1), which no source_ref carries), because a non-deferrable unique
-- index is checked row by row and a one-pass UPDATE could transiently collide with
-- a row it has not reached yet. typed_row_retirements needs nothing: nothing
-- retires substrate-lane rows. Rollback: not needed (the old key is the suffix
-- after the first ':' of the stream prefix).

CREATE TEMP TABLE eng1068_event_keys ON COMMIT DROP AS
SELECT e.id, e.namespace_id, substr(ev.source_id, 7) || ':' || e.source_ref AS new_ref
  FROM events e
  JOIN evidence ev ON ev.id = e.evidence_id
 WHERE ev.source_id LIKE 'event:%'
   AND ev.source_revision = ''
   AND e.source_ref <> ''
   AND left(e.source_ref, length(ev.source_id) - 5) <> substr(ev.source_id, 7) || ':';

DELETE FROM eng1068_event_keys k
 WHERE EXISTS (SELECT 1 FROM events x
                WHERE x.namespace_id = k.namespace_id AND x.source_ref = k.new_ref
                  AND x.id NOT IN (SELECT id FROM eng1068_event_keys))
    OR (SELECT count(*) FROM eng1068_event_keys d
         WHERE d.namespace_id = k.namespace_id AND d.new_ref = k.new_ref) > 1;

UPDATE events e SET source_ref = chr(1) || k.new_ref
  FROM eng1068_event_keys k WHERE e.id = k.id;
UPDATE events e SET source_ref = k.new_ref
  FROM eng1068_event_keys k WHERE e.id = k.id;

CREATE TEMP TABLE eng1068_observation_keys ON COMMIT DROP AS
SELECT o.id, o.namespace_id, o.occurred_at, substr(ev.source_id, 7) || ':' || o.source_ref AS new_ref
  FROM observations o
  JOIN evidence ev ON ev.id = o.evidence_id
 WHERE ev.source_id LIKE 'event:%'
   AND ev.source_revision = ''
   AND o.source_ref <> ''
   AND left(o.source_ref, length(ev.source_id) - 5) <> substr(ev.source_id, 7) || ':';

DELETE FROM eng1068_observation_keys k
 WHERE EXISTS (SELECT 1 FROM observations x
                WHERE x.namespace_id = k.namespace_id AND x.source_ref = k.new_ref
                  AND x.occurred_at = k.occurred_at
                  AND x.id NOT IN (SELECT id FROM eng1068_observation_keys))
    OR (SELECT count(*) FROM eng1068_observation_keys d
         WHERE d.namespace_id = k.namespace_id AND d.new_ref = k.new_ref
           AND d.occurred_at = k.occurred_at) > 1;

UPDATE observations o SET source_ref = chr(1) || k.new_ref
  FROM eng1068_observation_keys k WHERE o.id = k.id;
UPDATE observations o SET source_ref = k.new_ref
  FROM eng1068_observation_keys k WHERE o.id = k.id;
