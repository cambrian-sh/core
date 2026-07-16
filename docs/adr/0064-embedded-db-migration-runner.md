---
id: 0064
title: Embedded DB Migration Runner (schema_migrations, migrate subcommand, baseline stamping)
status: Accepted
date: 2026-07-15
supersedes: []
superseded_by: []
depends_on:
  - 0021-pgvector-schema-bootstrap
  - 0057-open-core-boundary
---

# ADR-0064: Embedded DB Migration Runner

## Status

Accepted

## Context

This is PLAT-02 (`distribution-production-readiness.md` §6 gap 1). Postgres schema is
managed two ways today, neither versioned:

- `PgVectorAdapter.ensureSchema` — a Go-embedded list of idempotent `CREATE … IF NOT
  EXISTS` / `ALTER … IF NOT EXISTS` / `CREATE OR REPLACE` statements, run on **every
  boot**, converging the DB to head. It carries the load-bearing **dimension-mismatch
  safety guard** (ADR-0021: a `VECTOR(dim)` change cannot silently drop the corpus).
- `db/migrations/002…011.sql` — raw SQL files applied **by hand**, with no version table.

There is no `schema_migrations` table, no `migrate` command, and nothing that refuses to
boot a binary against a DB that a newer binary already migrated. Every downstream
consumer (installer, K8s init containers, the benchmark "reset DB" flow) needs a runner.

## Decision

### A pure-Go runner (no goose/golang-migrate, no CGO)

`internal/migrate` is a ~single-file runner over the existing `*pgxpool.Pool`. We do
**not** vendor goose or golang-migrate: the runner is tiny, avoids a new dependency and
any CGO surface (project invariant), and needs the `${EMBEDDING_DIM}` templating and
dimension-guard integration that are simpler to own than to bend a general tool into.
All migrations — the head baseline included — are embedded via `go:embed
migrations/*.sql` (filenames `NNNN_name.sql`; `${EMBEDDING_DIM}` is substituted from
config). `schema_migrations` (`version BIGINT PK, name TEXT, applied_at TIMESTAMPTZ`)
records what has run. Each migration applies in its own transaction.

### `0001_baseline.sql` is the executed baseline; ensureSchema only guards the dimension

The head schema is a first-class migration, `migrations/0001_baseline.sql`, with
`${EMBEDDING_DIM}` as the vector-dimension placeholder. To avoid transcription drift, it
was **generated** from the former `ensureSchema` statement list (a throwaway renderer),
so the SQL is a faithful copy — and `postgres.BaselineStatements` was then **deleted**:
the SQL file is now the single source of truth. The runner applies `0001` like any
migration (executes it, records it); because every statement is idempotent
(`CREATE … IF NOT EXISTS` / `OR REPLACE` / `ADD COLUMN IF NOT EXISTS`), applying it to a
pre-existing database is a safe no-op that simply adopts the version table.

`ensureSchema` is reduced to the one thing that genuinely needs Go and config: the
**dimension-mismatch destructive guard** (ADR-0021). It runs the guard, then delegates
all schema creation to the runner. So:

- **Fresh DB** → the runner executes `0001` (+ deltas), creating everything.
- **Existing DB** (schema present, no version table) → the runner executes `0001`
  (idempotent no-op) and records v1 — the "adopt the version table" acceptance.
- **Dimension change** (guard drops `documents`/`edges` under explicit opt-in) → the guard
  also drops `schema_migrations`, so the runner re-applies the whole idempotent chain and
  recreates the tables at the new dimension.
- **DB ahead of binary** (max applied version > highest known) → the runner **refuses**,
  at boot and in the CLI.

### `migrate` subcommand + boot integration + flag

`cambrian-orchestrator migrate [up|status]` runs `postgres.RunMigrations` / prints status.
At boot, `ensureSchema` invokes the runner (after the dimension guard), gated by
`storage.auto_migrate` (default **true**, preserving today's create-schema-on-boot
behavior). Set false to have the operator own migrations via the subcommand / external
tooling; boot then runs only the dimension guard and no schema is created until `migrate
up` has run.

## Consequences

**Positive.**
- One versioned mechanism for forward schema change; external tooling (installer, K8s,
  benchmark reset) has a real `migrate` entry point.
- Boot refuses to run against a DB migrated by a newer binary — no silent schema skew.
- Zero new dependency, zero CGO, and **one source of truth**: the schema is
  `0001_baseline.sql`, executed by both boot and CLI; `BaselineStatements` was deleted, so
  there is no Go/SQL pair to drift.
- ensureSchema's dimension-mismatch guard is untouched, so the corpus-safety incident
  cannot regress; a destructive dimension change resets `schema_migrations` so the runner
  faithfully recreates the tables.
- A single versioned mechanism owns schema for good — future changes are `000N` deltas.

**Negative / costs.**
- `storage.auto_migrate=false` means boot no longer creates schema (only the dimension
  guard runs); the operator must run `migrate up` first. Default true preserves today's
  behavior.
- The runner is minimal (no down-migrations, no checksums). Forward-only up/status is the
  v1 scope; down-migrations are a follow-up if ever needed.
- The baseline is one large migration; a future schema change is a small `000N` delta, but
  editing the baseline itself (for a fresh-install-only change) requires care since it is
  only executed once per DB — the version table gates re-execution.

**Neutral.**
- The legacy `db/migrations/002…011.sql` files are retained as historical record; their
  content is already folded into `0001_baseline.sql`, so they are not re-embedded.

## References

- PLAT-02 (`docs/backlog/PLAT-02-migration-runner.md`);
  `distribution-production-readiness.md` §6 gap 1, §4.3 step 4.
- ADR-0021 (pgvector schema bootstrap + dimension guard), ADR-0057 (open-core boundary).
