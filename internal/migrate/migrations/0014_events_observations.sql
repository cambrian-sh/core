-- 0014_events_observations.sql — event-shaped knowledge (ADR-0108, Substrate Phase 4).
--
-- The deferred sub-types earn their existence: an EVENT is an n-ary occurrence whose
-- participants are ROWS with indexable entity edges (a receiver buried in a JSON value
-- cannot be traversed — the memo's §7B bug); an OBSERVATION is a high-volume
-- entity/predicate/value-at-time row that does NOT automatically become a semantic
-- knowledge item (§7E's corrected rule — promotion is a separate, deliberate lane).
--
-- NOTHING here is embedded or chunked; the phase gate depends on that.
-- ADDITIVE. Rollback: DROP TABLE event_roles; DROP TABLE events; DROP TABLE observations;

CREATE TABLE IF NOT EXISTS events (
	id           TEXT PRIMARY KEY,
	namespace_id TEXT NOT NULL DEFAULT 'default',
	-- The occurrence type ("gate_passage", "transfer"). DATA: the kernel never
	-- branches on a specific value.
	event_type   TEXT NOT NULL,
	occurred_at  TIMESTAMPTZ NOT NULL,
	evidence_id  TEXT REFERENCES evidence(id),
	-- Source-native reference; with the namespace it is the idempotency key, so a
	-- replayed delivery mints no second occurrence.
	source_ref   TEXT NOT NULL DEFAULT '',
	created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS events_source_ref_uniq
	ON events (namespace_id, source_ref) WHERE source_ref <> '';
CREATE INDEX IF NOT EXISTS idx_events_type_time ON events (namespace_id, event_type, occurred_at);

-- One row per participant. `role` names HOW the entity participated (sender,
-- receiver, vehicle, gate); the entity edge is a real, indexable column.
CREATE TABLE IF NOT EXISTS event_roles (
	event_id  TEXT NOT NULL REFERENCES events(id) ON DELETE CASCADE,
	role      TEXT NOT NULL,
	entity_id TEXT NOT NULL,
	PRIMARY KEY (event_id, role, entity_id)
);

CREATE INDEX IF NOT EXISTS idx_event_roles_entity ON event_roles (entity_id, role);

-- Partitioned from day one (ADR-0108 D1): observations are the substrate's
-- high-volume table, and retrofitting partitioning onto a populated table is the
-- expensive path. RANGE on occurred_at with a DEFAULT partition means the layout is
-- real now and adding monthly partitions later is plain DDL.
CREATE TABLE IF NOT EXISTS observations (
	id              BIGINT GENERATED ALWAYS AS IDENTITY,
	namespace_id    TEXT NOT NULL DEFAULT 'default',
	entity_id       TEXT NOT NULL,
	predicate       TEXT NOT NULL,
	value_type      TEXT NOT NULL,
	value_date      TIMESTAMPTZ,
	value_number    DOUBLE PRECISION,
	value_text      TEXT,
	value_entity_id TEXT,
	-- Where the observation places the entity, when the source says so
	-- ("camera:gate-3"). A convenience column, not an entity edge — a location
	-- that must be traversable belongs in an event role.
	location        TEXT NOT NULL DEFAULT '',
	occurred_at     TIMESTAMPTZ NOT NULL,
	confidence      DOUBLE PRECISION NOT NULL DEFAULT 1.0,
	evidence_id     TEXT,
	source_ref      TEXT NOT NULL DEFAULT '',
	created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (id, occurred_at),
	CONSTRAINT observations_exactly_one_value
		CHECK (num_nonnulls(value_date, value_number, value_text, value_entity_id) = 1)
) PARTITION BY RANGE (occurred_at);

CREATE TABLE IF NOT EXISTS observations_default PARTITION OF observations DEFAULT;

-- Replay protection. Partitioned unique indexes must include the partition key, so
-- idempotency is (namespace, source_ref, occurred_at) — a replayed delivery carries
-- the same occurred_at, so the guarantee is intact.
CREATE UNIQUE INDEX IF NOT EXISTS observations_source_ref_uniq
	ON observations (namespace_id, source_ref, occurred_at) WHERE source_ref <> '';

-- The point-lookup / history index: latest-for-entity and range scans.
CREATE INDEX IF NOT EXISTS idx_observations_entity_time
	ON observations (namespace_id, entity_id, predicate, occurred_at DESC);
