#!/bin/sh
# scripts/restore-bbolt.sh
# Restore Cambrian BBolt databases and the content-store blob directory from a
# backup tarball created by backup-bbolt.sh.
#
# WARNING: This is DESTRUCTIVE. It overwrites:
#   data/agents.db
#   data/content_store.db
#   data/content_store_blobs/
#
# The Cambrian runtime (orchestrator) MUST be stopped before running this
# script. BBolt is single-writer; restoring into a live database will corrupt
# it or fail with a file-lock error.
#
# Safety:
#   Requires --force flag or interactive confirmation. Without it, the script
#   prints a warning and exits.
#
# Usage:
#   ./scripts/restore-bbolt.sh --force <backup-file.tar.gz>

set -euo pipefail

# ── Configuration ───────────────────────────────────────────────────────────

DATA_DIR="data"
AGENTS_DB="${DATA_DIR}/agents.db"
CONTENT_DB="${DATA_DIR}/content_store.db"
BLOBS_DIR="${DATA_DIR}/content_store_blobs"

FORCE=0
BACKUP_FILE=""

# ── Argument parsing ────────────────────────────────────────────────────────

for arg in "$@"; do
  case "${arg}" in
    --force)
      FORCE=1
      ;;
    *.tar.gz)
      BACKUP_FILE="${arg}"
      ;;
  esac
done

# ── Validation ────────────────────────────────────────────────────────────────

die() {
  printf '%s\n' "$1" >&2
  exit 1
}

if [ -z "${BACKUP_FILE}" ]; then
  die "Usage: $0 --force <backup-file.tar.gz>"
fi

if [ ! -f "${BACKUP_FILE}" ]; then
  die "ERROR: Backup file not found: ${BACKUP_FILE}"
fi

# ── Safety guard ────────────────────────────────────────────────────────────

if [ "${FORCE}" -ne 1 ]; then
  printf '\n'
  printf 'WARNING: This will OVERWRITE the live BBolt databases and blob store.\n'
  printf '  -> %s\n' "${AGENTS_DB}"
  printf '  -> %s\n' "${CONTENT_DB}"
  printf '  -> %s\n' "${BLOBS_DIR}"
  printf 'The Cambrian runtime MUST be stopped before proceeding.\n'
  printf '\n'
  printf 'To proceed, re-run with --force:\n'
  printf '  %s --force %s\n' "$0" "${BACKUP_FILE}"
  printf '\n'
  exit 1
fi

printf 'Proceeding with DESTRUCTIVE restore from %s\n' "${BACKUP_FILE}"

# ── Restore ─────────────────────────────────────────────────────────────────

TMPDIR="$(mktemp -d)"
trap 'rm -rf "${TMPDIR}"' EXIT

printf 'Extracting tarball...\n'
tar xzf "${BACKUP_FILE}" -C "${TMPDIR}"

# Verify expected contents
if [ ! -f "${TMPDIR}/agents.db" ]; then
  die "ERROR: Backup tarball missing agents.db"
fi
if [ ! -f "${TMPDIR}/content_store.db" ]; then
  die "ERROR: Backup tarball missing content_store.db"
fi

# Ensure data directory exists
mkdir -p "${DATA_DIR}"

# Move current files to .old as a last-rescue guard (overwrite any previous .old)
if [ -f "${AGENTS_DB}" ]; then
  mv -f "${AGENTS_DB}" "${AGENTS_DB}.old"
  printf 'Moved existing %s -> %s.old\n' "${AGENTS_DB}" "${AGENTS_DB}"
fi
if [ -f "${CONTENT_DB}" ]; then
  mv -f "${CONTENT_DB}" "${CONTENT_DB}.old"
  printf 'Moved existing %s -> %s.old\n' "${CONTENT_DB}" "${CONTENT_DB}"
fi
if [ -d "${BLOBS_DIR}" ]; then
  mv -f "${BLOBS_DIR}" "${BLOBS_DIR}.old"
  printf 'Moved existing %s -> %s.old\n' "${BLOBS_DIR}" "${BLOBS_DIR}"
fi

# Copy restored files into place
printf 'Restoring database files...\n'
cp "${TMPDIR}/agents.db" "${AGENTS_DB}"
cp "${TMPDIR}/content_store.db" "${CONTENT_DB}"

if [ -d "${TMPDIR}/content_store_blobs" ]; then
  mkdir -p "${BLOBS_DIR}"
  cp -r "${TMPDIR}/content_store_blobs"/* "${BLOBS_DIR}/" 2>/dev/null || true
fi

printf 'Restore complete.\n'
printf '  %s\n' "${AGENTS_DB}"
printf '  %s\n' "${CONTENT_DB}"
printf '  %s\n' "${BLOBS_DIR}"
printf '\n'
printf 'If the restore is successful, you may delete the .old files:\n'
printf '  rm -f %s.old %s.old %s.old\n' "${AGENTS_DB}" "${CONTENT_DB}" "${BLOBS_DIR}"
