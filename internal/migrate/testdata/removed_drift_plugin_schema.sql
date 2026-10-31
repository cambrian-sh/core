-- Fixture: the runtime schema of the removed drift plugin, verbatim from
-- cambrian-premium records/store_pg.go (schemaSQL) as last committed (2fa2c3d), before its
-- deletion on 2026-10-31. Used only by drop_removed_plugin_tables_test.go to prove
-- 0017 removes everything it created. Do not edit to make a test pass.

CREATE TABLE IF NOT EXISTS drift_records (
    id            BIGSERIAL   PRIMARY KEY,
    kind          TEXT        NOT NULL,
    record_key    TEXT        NOT NULL,
    owner         TEXT        NOT NULL DEFAULT '',
    attributes    JSONB       NOT NULL DEFAULT '{}'::jsonb,
    tags          TEXT[]      NOT NULL DEFAULT '{}',
    valid_from    TIMESTAMPTZ NOT NULL,
    observed_at   TIMESTAMPTZ NOT NULL,
    superseded_at TIMESTAMPTZ,
    push_id       TEXT        NOT NULL,
    transport     TEXT        NOT NULL,
    received_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The invariant that makes Get meaningful, enforced by the database rather than
-- by convention: at most ONE live version per (kind, record_key). A partial
-- unique index is the right instrument because superseded rows are unconstrained.
CREATE UNIQUE INDEX IF NOT EXISTS drift_records_current_uniq
    ON drift_records (kind, record_key) WHERE superseded_at IS NULL;

-- History for one key, newest first: what an alert reads to say "slipped twice".
CREATE INDEX IF NOT EXISTS drift_records_kind_key_idx
    ON drift_records (kind, record_key, valid_from DESC);

-- "What is currently open for this customer."
CREATE INDEX IF NOT EXISTS drift_records_owner_current_idx
    ON drift_records (kind, owner) WHERE superseded_at IS NULL;

-- One row per accepted push. Its job is idempotency: a push_id already present is
-- a no-op, which matters because a nightly ERP export is exactly the thing that
-- gets retried. It doubles as an import audit — how many rows, which transport,
-- when — the first thing anyone asks when a value looks wrong.
CREATE TABLE IF NOT EXISTS drift_record_pushes (
    push_id      TEXT        PRIMARY KEY,
    transport    TEXT        NOT NULL,
    record_count INTEGER     NOT NULL,
    received_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One row per served command_id. This is the SECOND idempotency key and it
-- catches what push_id cannot: a client that regenerates its push_id on retry,
-- which an unattended exporter written by someone else eventually will. Kept in
-- the plugin rather than reusing the operator audit store because KernelServices
-- exposes no audit seam to a plugin, and inventing one in core to save a table
-- would be the tail wagging the dog.
CREATE TABLE IF NOT EXISTS drift_commands (
    command_id  TEXT        PRIMARY KEY,
    claimed_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Aging the command log needs this; without it a prune is a sequential scan of
-- every command ever served.
CREATE INDEX IF NOT EXISTS drift_commands_claimed_idx ON drift_commands (claimed_at);

-- Superseded versions are pruned by (kind, superseded_at). Without this index the
-- retention pass scans the whole table on every tick, which is how a compactor
-- becomes the load it was added to prevent.
CREATE INDEX IF NOT EXISTS drift_records_superseded_idx
    ON drift_records (kind, superseded_at) WHERE superseded_at IS NOT NULL;

-- One row per compaction pass. Silent data loss in an audit product is a
-- contradiction, so what was dropped and when is itself durable. Read back over
-- the plugin's own plane rather than the operator feed: the kernel's feed bridge
-- subscribes to specific event types, so a plugin event would need a CORE change
-- to appear there, and the plugin owning its observability is the whole point of
-- owning its plane.
CREATE TABLE IF NOT EXISTS drift_retention_runs (
    id               BIGSERIAL   PRIMARY KEY,
    started_at       TIMESTAMPTZ NOT NULL,
    finished_at      TIMESTAMPTZ NOT NULL,
    versions_deleted INTEGER     NOT NULL DEFAULT 0,
    commands_deleted INTEGER     NOT NULL DEFAULT 0,
    bounded          BOOLEAN     NOT NULL DEFAULT false,
    error            TEXT        NOT NULL DEFAULT ''
);

-- One row per REFUSED derivation (ADR-0095 D9). The refusal must leave a trace or
-- "suppressed by policy" and "nothing found" look identical to an operator, and a
-- detection product whose silences are indistinguishable from its successes is
-- not one anybody should trust.
CREATE TABLE IF NOT EXISTS drift_refusals (
    id        BIGSERIAL   PRIMARY KEY,
    at        TIMESTAMPTZ NOT NULL,
    kind      TEXT        NOT NULL,
    ref       TEXT        NOT NULL DEFAULT '',
    boundary  TEXT[]      NOT NULL DEFAULT '{}',
    reason    TEXT        NOT NULL
);

CREATE INDEX IF NOT EXISTS drift_refusals_at_idx ON drift_refusals (at DESC);

-- One row per raised drift alert. Durable rather than emitted-and-forgotten for
-- three reasons that arrived together: a console cannot render what nothing
-- stores, a benchmark cannot measure what it cannot read back, and retention had
-- no citation floor to consult while alerts were ephemeral.
--
-- cited_versions is the floor itself. It holds drift_records ids rather than keys
-- because an alert quotes a VERSION -- "it has moved 3 times" is a claim about
-- superseded rows, and a floor expressed over keys would let the compactor prune
-- exactly the evidence that makes the claim checkable.
CREATE TABLE IF NOT EXISTS drift_alerts (
    id             BIGSERIAL   PRIMARY KEY,
    kind           TEXT        NOT NULL,
    record_key     TEXT        NOT NULL,
    owner          TEXT        NOT NULL DEFAULT '',
    speaker        TEXT        NOT NULL DEFAULT '',
    message_id     TEXT        NOT NULL DEFAULT '',
    delta_days     INTEGER     NOT NULL DEFAULT 0,
    promised       TIMESTAMPTZ,
    recorded       TIMESTAMPTZ,
    slip_count     INTEGER     NOT NULL DEFAULT 0,
    text           TEXT        NOT NULL DEFAULT '',
    citations      JSONB       NOT NULL DEFAULT '[]'::jsonb,
    tags           TEXT[]      NOT NULL DEFAULT '{}',
    cited_versions BIGINT[]    NOT NULL DEFAULT '{}',
    raised_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    detect_ms      BIGINT      NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS drift_alerts_raised_idx ON drift_alerts (raised_at DESC);

-- The citation floor is read on every compaction pass, so it must not be a
-- sequential scan of every alert ever raised.
CREATE INDEX IF NOT EXISTS drift_alerts_cited_idx ON drift_alerts USING GIN (cited_versions);

-- One row per extracted commitment. Durable for one reason: RETRACTION.
--
-- "Actually, make that the 20th" is the most common real-world shape and the
-- easiest to get wrong, and it only works if a later commitment can supersede an
-- earlier one from the same speaker about the same record. Messages arrive one at
-- a time from a chat connector, so holding commitments in memory for the duration
-- of one batch would make retraction work in a benchmark and fail in production --
-- the worst possible split.
--
-- Commitments supersede exactly as records do, and for the same reason: the
-- earlier promise is not wrong, it WAS true, and an audit trail that overwrites it
-- cannot answer "when did they change their mind".
CREATE TABLE IF NOT EXISTS drift_commitments (
    id             BIGSERIAL   PRIMARY KEY,
    kind           TEXT        NOT NULL,
    record_ref     TEXT        NOT NULL,
    speaker        TEXT        NOT NULL,
    counterparty   TEXT        NOT NULL DEFAULT '',
    source_message TEXT        NOT NULL,
    committed_date TIMESTAMPTZ NOT NULL,
    tags           TEXT[]      NOT NULL DEFAULT '{}',
    observed_at    TIMESTAMPTZ NOT NULL,
    superseded_at  TIMESTAMPTZ,
    received_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- At most one LIVE commitment per (kind, record_ref, speaker). The same partial
-- unique index that makes Get meaningful for records makes "what did they last
-- say" meaningful here.
CREATE UNIQUE INDEX IF NOT EXISTS drift_commitments_current_uniq
    ON drift_commitments (kind, record_ref, speaker) WHERE superseded_at IS NULL;

-- Replay protection: the same message must not produce a second commitment.
CREATE UNIQUE INDEX IF NOT EXISTS drift_commitments_message_uniq
    ON drift_commitments (source_message, record_ref);
;
