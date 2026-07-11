#!/bin/sh
# scripts/restore-postgres.sh
# Restore the Cambrian Postgres database from a gzip-compressed SQL dump.
#
# WARNING: This is DESTRUCTIVE. It drops and recreates the target database.
# The Cambrian runtime (orchestrator) MUST be stopped before running this script
# to avoid concurrent writes and connection conflicts.
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
#   psql via "docker exec" into the named container.
#   Otherwise it runs psql locally.
#
# Safety:
#   Requires --force flag or interactive confirmation. Without it, the script
#   prints a warning and exits.
#
# Usage:
#   ./scripts/restore-postgres.sh [--force] [CONTAINER_NAME] <backup-file.sql.gz>
#   CONTAINER_NAME=cambrian-db ./scripts/restore-postgres.sh --force backup.sql.gz

set -euo pipefail

# ── Configuration ───────────────────────────────────────────────────────────

HOST="${CAMBRIAN_DATABASE__HOST:-localhost}"
PORT="${CAMBRIAN_DATABASE__PORT:-5432}"
USER="${CAMBRIAN_DATABASE__USER:-cambrian}"
PASSWORD="${CAMBRIAN_DATABASE__PASSWORD:-}"
DBNAME="${CAMBRIAN_DATABASE__DBNAME:-cambrian_db}"

FORCE=0
CONTAINER_NAME="${CONTAINER_NAME:-}"
BACKUP_FILE=""

# ── Argument parsing ────────────────────────────────────────────────────────

for arg in "$@"; do
  case "${arg}" in
    --force)
      FORCE=1
      ;;
    *.sql.gz)
      BACKUP_FILE="${arg}"
      ;;
    *)
      # If it looks like a container name and no container name set yet, use it
      if [ -z "${CONTAINER_NAME}" ] && [ "${arg}" != "--force" ]; then
        CONTAINER_NAME="${arg}"
      fi
      ;;
  esac
done

# ── Validation ────────────────────────────────────────────────────────────────

die() {
  printf '%s\n' "$1" >&2
  exit 1
}

if [ -z "${BACKUP_FILE}" ]; then
  die "Usage: $0 [--force] [CONTAINER_NAME] <backup-file.sql.gz>"
fi

if [ ! -f "${BACKUP_FILE}" ]; then
  die "ERROR: Backup file not found: ${BACKUP_FILE}"
fi

if [ -n "${CONTAINER_NAME}" ]; then
  if ! docker inspect "${CONTAINER_NAME}" >/dev/null 2>&1; then
    die "ERROR: Docker container '${CONTAINER_NAME}' not found."
  fi
else
  if ! command -v psql >/dev/null 2>&1; then
    die "ERROR: psql not found in PATH. Install PostgreSQL client tools or set CONTAINER_NAME."
  fi
fi

# ── Safety guard ────────────────────────────────────────────────────────────

if [ "${FORCE}" -ne 1 ]; then
  printf '\n'
  printf 'WARNING: This will DROP and RECREATE database "%s".\n' "${DBNAME}"
  printf 'All existing data in that database will be lost.\n'
  printf 'The Cambrian runtime MUST be stopped before proceeding.\n'
  printf '\n'
  printf 'To proceed, re-run with --force:\n'
  printf '  %s --force %s\n' "$0" "$*"
  printf '\n'
  exit 1
fi

printf 'Proceeding with DESTRUCTIVE restore of "%s" from %s\n' "${DBNAME}" "${BACKUP_FILE}"

# ── Restore ─────────────────────────────────────────────────────────────────

export PGPASSWORD="${PASSWORD}"

# Drop and recreate the database. Connect to 'postgres' maintenance DB to do so.
if [ -n "${CONTAINER_NAME}" ]; then
  printf 'Dropping and recreating DB via container "%s"...\n' "${CONTAINER_NAME}"
  docker exec \
    -e PGPASSWORD="${PASSWORD}" \
    "${CONTAINER_NAME}" \
    psql -h localhost -p 5432 -U "${USER}" -d postgres -c "DROP DATABASE IF EXISTS \"${DBNAME}\";"
  docker exec \
    -e PGPASSWORD="${PASSWORD}" \
    "${CONTAINER_NAME}" \
    psql -h localhost -p 5432 -U "${USER}" -d postgres -c "CREATE DATABASE \"${DBNAME}\";"

  printf 'Restoring dump into container...\n'
  gunzip -c "${BACKUP_FILE}" | docker exec -i \
    -e PGPASSWORD="${PASSWORD}" \
    "${CONTAINER_NAME}" \
    psql -h localhost -p 5432 -U "${USER}" -d "${DBNAME}"
else
  printf 'Dropping and recreating DB on %s:%s...\n' "${HOST}" "${PORT}"
  psql -h "${HOST}" -p "${PORT}" -U "${USER}" -d postgres -c "DROP DATABASE IF EXISTS \"${DBNAME}\";"
  psql -h "${HOST}" -p "${PORT}" -U "${USER}" -d postgres -c "CREATE DATABASE \"${DBNAME}\";"

  printf 'Restoring dump...\n'
  gunzip -c "${BACKUP_FILE}" | psql -h "${HOST}" -p "${PORT}" -U "${USER}" -d "${DBNAME}"
fi

unset PGPASSWORD

printf 'Restore complete: %s -> %s\n' "${BACKUP_FILE}" "${DBNAME}"
