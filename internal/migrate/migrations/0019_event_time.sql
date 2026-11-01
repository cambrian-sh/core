-- 0019_event_time.sql — event time on memory documents and chunks (ADR-0134 P0).
--
-- Two clocks, never confused:
--   * event time  — when the content happened (valid time): event_time, with
--                   event_time_end for a multi-part span, time_source saying where it
--                   came from, time_precision saying how exact it is, claimed_time
--                   keeping a caller's clamped future time (ADR-0134 Q3/Q4/Q5).
--   * ingest time — when Cambrian recorded it: the existing created_at.
-- The vocabularies of time_source / time_precision are defined once, in
-- cambrian-core/domain/event_time.go (owner decision Q9); no CHECK constraint copies
-- them here.
--
-- SAFE ON A LARGE STORE. Every ALTER below is catalog-only:
--   * ADD COLUMN of a nullable column with NO default does not rewrite the table
--     (PostgreSQL >= 11), so there is no full-table rewrite of chunks under lock and
--     no HNSW churn (the gotcha recorded in ADR-0134: the runner wraps this file in
--     one transaction, internal/migrate/migrate.go applyMigration).
--   * There is NO backfill. Old rows keep NULL and every reader takes
--     COALESCE(event_time, created_at AT TIME ZONE 'UTC') with a NULL time_source
--     reading as ingest_default (ADR-0134 D2 as corrected by the blast-radius study).
--   * ALTER COLUMN ... SET DEFAULT changes only future inserts.
--
-- The b-tree indexes the event-time window needs are NOT built here: CREATE INDEX
-- CONCURRENTLY cannot run inside the transaction this file is applied in. The runner
-- builds them afterwards, out of band (migrate.go ensureConcurrentIndexes).
--
-- The descriptor tables (tools, skills, agent_profiles) get the same columns. They
-- never carry an event time, but the adapter reads every retrievable table through ONE
-- column list (pgvector_adapter.go documentColumns / scanDocument;
-- row_shape_test.go), and a column that exists on chunks but not on them would break
-- GetByID / GetBatch for any id that lives there (the bug 0008 fixed for section_path).

ALTER TABLE chunks
	ADD COLUMN IF NOT EXISTS event_time     TIMESTAMPTZ,
	ADD COLUMN IF NOT EXISTS event_time_end TIMESTAMPTZ,
	ADD COLUMN IF NOT EXISTS time_source    TEXT,
	ADD COLUMN IF NOT EXISTS time_precision TEXT,
	ADD COLUMN IF NOT EXISTS claimed_time   TIMESTAMPTZ;

ALTER TABLE documents
	ADD COLUMN IF NOT EXISTS event_time     TIMESTAMPTZ,
	ADD COLUMN IF NOT EXISTS event_time_end TIMESTAMPTZ,
	ADD COLUMN IF NOT EXISTS time_source    TEXT,
	ADD COLUMN IF NOT EXISTS time_precision TEXT,
	ADD COLUMN IF NOT EXISTS claimed_time   TIMESTAMPTZ;

ALTER TABLE tools
	ADD COLUMN IF NOT EXISTS event_time     TIMESTAMPTZ,
	ADD COLUMN IF NOT EXISTS event_time_end TIMESTAMPTZ,
	ADD COLUMN IF NOT EXISTS time_source    TEXT,
	ADD COLUMN IF NOT EXISTS time_precision TEXT,
	ADD COLUMN IF NOT EXISTS claimed_time   TIMESTAMPTZ;

ALTER TABLE skills
	ADD COLUMN IF NOT EXISTS event_time     TIMESTAMPTZ,
	ADD COLUMN IF NOT EXISTS event_time_end TIMESTAMPTZ,
	ADD COLUMN IF NOT EXISTS time_source    TEXT,
	ADD COLUMN IF NOT EXISTS time_precision TEXT,
	ADD COLUMN IF NOT EXISTS claimed_time   TIMESTAMPTZ;

ALTER TABLE agent_profiles
	ADD COLUMN IF NOT EXISTS event_time     TIMESTAMPTZ,
	ADD COLUMN IF NOT EXISTS event_time_end TIMESTAMPTZ,
	ADD COLUMN IF NOT EXISTS time_source    TEXT,
	ADD COLUMN IF NOT EXISTS time_precision TEXT,
	ADD COLUMN IF NOT EXISTS claimed_time   TIMESTAMPTZ;

-- The UTC convention for the ingest clock (ADR-0134 Q3; the limitation recorded on
-- the ADR: memory tables use TIMESTAMP WITHOUT TIME ZONE). created_at holds a UTC wall
-- clock: the Go writer sends UTC (goqu renders times in UTC) and pgx reads the column
-- back as UTC. The DEFAULT used to be NOW(), whose wall clock follows the SESSION time
-- zone, so a server whose TimeZone is not UTC would have stored local time. Readers
-- compare it with event_time as created_at AT TIME ZONE 'UTC'.
ALTER TABLE chunks    ALTER COLUMN created_at SET DEFAULT (now() AT TIME ZONE 'UTC');
ALTER TABLE documents ALTER COLUMN created_at SET DEFAULT (now() AT TIME ZONE 'UTC');
ALTER TABLE documents ALTER COLUMN updated_at SET DEFAULT (now() AT TIME ZONE 'UTC');
