#!/bin/sh
# scripts/backup-postgres.sh
# Backup the Cambrian Postgres database to a timestamped gzip-compressed SQL dump.
#
# Reads connection parameters from the same env vars the runtime uses:
#   CAMBRIAN_DATABASE__HOST     (default: localhost)
#   CAMBRIAN_DATABASE__PORT     (default: 5432)
#   CAMBRIAN_DATABASE__USER     (default: cambrian)
#   CAMBRIAN_DATABASE__PASSWORD (default: empty)
#   CAMBRIAN_DATABASE__DBNAME   (default: cambrian_db)
#
# Mode selection:
#   If CONTAINER_NAME is set (env or first positional arg), the script runs
#   pg_dump via "docker exec" into the named container. This is the normal
#   mode when Postgres was started with db/docker-compose.yml (container_name:
#   cambrian-db).
#   Otherwise it runs pg_dump locally against a running server.
#
# Rotation:
#   Keeps the most recent 7 backups in backups/postgres/; older files are
#   removed automatically.
#
# Usage:
#   ./scripts/backup-postgres.sh [CONTAINER_NAME]
#   CONTAINER_NAME=cambrian-db ./scripts/backup-postgres.sh

set -euo pipefail

# ── Configuration ───────────────────────────────────────────────────────────

HOST="${CAMBRIAN_DATABASE__HOST:-localhost}"
PORT="${CAMBRIAN_DATABASE__PORT:-5432}"
USER="${CAMBRIAN_DATABASE__USER:-cambrian}"
PASSWORD="${CAMBRIAN_DATABASE__PASSWORD:-}"
DBNAME="${CAMBRIAN_DATABASE__DBNAME:-cambrian_db}"

# Allow CONTAINER_NAME as first positional argument or env var
CONTAINER_NAME="${CONTAINER_NAME:-${1:-}}"

BACKUP_DIR="backups/postgres"
KEEP_COUNT=7

TS="$(date +%Y%m%d-%H%M%S)"
OUTFILE="${BACKUP_DIR}/cambrian-${TS}.sql.gz"

# ── Helpers ─────────────────────────────────────────────────────────────────

die() {
  printf '%s\n' "$1" >&2
  exit 1
}

# ── Preconditions ───────────────────────────────────────────────────────────

mkdir -p "${BACKUP_DIR}"

if [ -n "${CONTAINER_NAME}" ]; then
  if ! docker inspect "${CONTAINER_NAME}" >/dev/null 2>&1; then
    die "ERROR: Docker container '${CONTAINER_NAME}' not found."
  fi
  if ! docker exec "${CONTAINER_NAME}" pg_dump --version >/dev/null 2>&1; then
    die "ERROR: pg_dump not available inside container '${CONTAINER_NAME}'."
  fi
else
  if ! command -v pg_dump >/dev/null 2>&1; then
    die "ERROR: pg_dump not found in PATH. Install PostgreSQL client tools or set CONTAINER_NAME."
  fi
fi

# ── Dump ────────────────────────────────────────────────────────────────────

export PGPASSWORD="${PASSWORD}"

if [ -n "${CONTAINER_NAME}" ]; then
  printf 'Backing up DB "%s" from container "%s" -> %s\n' "${DBNAME}" "${CONTAINER_NAME}" "${OUTFILE}"
  # shellcheck disable=SC2024
  docker exec \
    -e PGPASSWORD="${PASSWORD}" \
    "${CONTAINER_NAME}" \
    pg_dump -h localhost -p 5432 -U "${USER}" -d "${DBNAME}" --no-owner --clean --if-exists \
    | gzip > "${OUTFILE}"
else
  printf 'Backing up DB "%s" from %s:%s -> %s\n' "${DBNAME}" "${HOST}" "${PORT}" "${OUTFILE}"
  pg_dump -h "${HOST}" -p "${PORT}" -U "${USER}" -d "${DBNAME}" --no-owner --clean --if-exists \
    | gzip > "${OUTFILE}"
fi

unset PGPASSWORD

# ── Verify ──────────────────────────────────────────────────────────────────

if [ ! -s "${OUTFILE}" ]; then
  die "ERROR: Backup file is empty or missing."
fi

SIZE="$(du -h "${OUTFILE}" | cut -f1)"
printf 'Backup complete: %s (%s)\n' "${OUTFILE}" "${SIZE}"

# ── Rotation ────────────────────────────────────────────────────────────────

# Keep only the most recent KEEP_COUNT .sql.gz files, sorted by filename (timestamped)
COUNT="$(find "${BACKUP_DIR}" -maxdepth 1 -name 'cambrian-*.sql.gz' -type f | sort | wc -l)"
if [ "${COUNT}" -gt "${KEEP_COUNT}" ]; then
  find "${BACKUP_DIR}" -maxdepth 1 -name 'cambrian-*.sql.gz' -type f | sort | head -n -"${KEEP_COUNT}" | while IFS= read -r f; do
    printf 'Rotating out old backup: %s\n' "${f}"
    rm -f "${f}"
  done
fi

printf 'Done. %d backup(s) retained in %s.\n' "${KEEP_COUNT}" "${BACKUP_DIR}"
