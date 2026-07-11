# Cambrian Backup & Restore Guide

This directory contains production backup and restore scripts for the Cambrian
runtime. There are two independent persistence layers: **Postgres** (vector +
relational data) and **BBolt** (agent registry, events, checkpoints, content
store). Both must be backed up regularly.

## Files

| Script | Purpose |
|--------|---------|
| `backup-postgres.sh`  | `pg_dump` of the Cambrian database to a timestamped `.sql.gz` |
| `restore-postgres.sh` | Drop/recreate the DB and restore from a `.sql.gz` dump |
| `backup-bbolt.sh`     | Consistent backup of `data/agents.db`, `data/content_store.db`, and `data/content_store_blobs/` into a tarball |
| `restore-bbolt.sh`    | Restore BBolt files and blobs from a tarball |

## Prerequisites

- **Postgres client tools** (`pg_dump`, `psql`) installed on the host, **or**
  Docker with access to the `cambrian-db` container.
- **bbolt CLI** (optional but recommended). Install with:
  ```bash
  go install go.etcd.io/bbolt/cmd/bbolt@latest
  ```
  If the CLI is missing, the BBolt backup script falls back to `cp` with a
  prominent warning that the runtime must be stopped first.
- The scripts read the same environment variables the runtime uses:
  - `CAMBRIAN_DATABASE__HOST` (default: `localhost`)
  - `CAMBRIAN_DATABASE__PORT` (default: `5432`)
  - `CAMBRIAN_DATABASE__USER` (default: `cambrian`)
  - `CAMBRIAN_DATABASE__PASSWORD` (default: empty)
  - `CAMBRIAN_DATABASE__DBNAME` (default: `cambrian_db`)

## Running backups

### From the Docker Compose host (recommended)

When Postgres is running via `db/docker-compose.yml`:

```bash
# Postgres backup (uses docker exec into the container)
CONTAINER_NAME=cambrian-db ./scripts/backup-postgres.sh

# BBolt backup (run from the repo root where data/ lives)
./scripts/backup-bbolt.sh
```

### From a remote host with direct DB access

```bash
# Postgres backup (uses local pg_dump)
CAMBRIAN_DATABASE__HOST=db.example.com \
CAMBRIAN_DATABASE__PASSWORD=secret \
  ./scripts/backup-postgres.sh

# BBolt backup (must run on the host that owns the data/ directory)
./scripts/backup-bbolt.sh
```

## Scheduling with cron

A typical nightly backup crontab (run from the repo root):

```cron
# Every day at 02:00 — backup both layers
0 2 * * * cd /opt/cambrian/core && CONTAINER_NAME=cambrian-db ./scripts/backup-postgres.sh >> /var/log/cambrian-backup.log 2>&1
0 2 * * * cd /opt/cambrian/core && ./scripts/backup-bbolt.sh >> /var/log/cambrian-backup.log 2>&1
```

Backups are written to:
- `backups/postgres/cambrian-<timestamp>.sql.gz`
- `backups/bbolt/cambrian-bbolt-<timestamp>.tar.gz`

Both scripts rotate old backups automatically, keeping the **7 most recent**
files.

## Restoring

> **CRITICAL:** Stop the Cambrian orchestrator before any restore operation.
> The runtime holds BBolt file locks and maintains open Postgres connections.
> Restoring while it is running will corrupt data or cause connection errors.

### Postgres restore

```bash
# 1. Stop the runtime
#    (e.g., docker compose down, or systemctl stop cambrian)

# 2. Restore from a specific backup
./scripts/restore-postgres.sh --force cambrian-db backups/postgres/cambrian-20250711-020000.sql.gz
```

The script drops and recreates the target database, then replays the dump.

### BBolt restore

```bash
# 1. Stop the runtime

# 2. Restore from a specific backup
./scripts/restore-bbolt.sh --force backups/bbolt/cambrian-bbolt-20250711-020000.tar.gz
```

The script preserves the previous database files as `.old` copies. After you
confirm the restore is healthy, delete them:

```bash
rm -f data/agents.db.old data/content_store.db.old data/content_store_blobs.old
```

## Docker-level volume snapshots

In addition to logical backups, the Postgres data volume (`cambrian_data`) can
be snapshotted at the Docker level:

```bash
# Stop the DB container first for a clean snapshot
docker compose -f db/docker-compose.yml stop cambrian-db
docker run --rm -v cambrian_data:/data -v $(pwd)/backups:/out alpine \
  tar czf /out/cambrian_data-volume-<date>.tar.gz -C /data .
docker compose -f db/docker-compose.yml start cambrian-db
```

Volume snapshots are faster for full-system recovery but less granular than
`pg_dump` for single-table restores or migration to a new host.

## Safety notes

- **Never** run restore scripts without `--force`. They require explicit
  opt-in because they destroy live data.
- **Never** run `backup-bbolt.sh` while the runtime is active unless the
  `bbolt backup` CLI is installed. The fallback `cp` method copies the raw
  file and will produce a corrupt backup if the database is open.
- Passwords are passed via the `PGPASSWORD` environment variable; they never
  appear in process listings (`ps`).
