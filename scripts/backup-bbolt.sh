#!/bin/sh
# scripts/backup-bbolt.sh
# Backup Cambrian BBolt databases and the content-store blob directory.
#
# BBolt is single-writer / file-locked. The SAFE online backup method is the
# "bbolt backup" command, which creates a consistent snapshot while the DB is
# open. If the bbolt CLI is not installed, the script falls back to plain "cp",
# but prints a PROMINENT WARNING that the Cambrian runtime MUST be stopped first
# to avoid copying a partially-written or locked file.
#
# Files backed up:
#   data/agents.db            -> agents.db
#   data/content_store.db     -> content_store.db
#   data/content_store_blobs/ -> content_store_blobs/
#
# Output:
#   backups/bbolt/cambrian-bbolt-<timestamp>.tar.gz
#
# Rotation:
#   Keeps the most recent 7 tarballs; older files are removed automatically.
#
# Prerequisites:
#   - bbolt CLI (optional; from github.com/etcd-io/bbolt/cmd/bbolt)
#   - OR: Go toolchain to run "go run go.etcd.io/bbolt/cmd/bbolt"
#   - OR: stop the runtime and let the script fall back to cp
#
# Usage:
#   ./scripts/backup-bbolt.sh

set -euo pipefail

# ── Configuration ───────────────────────────────────────────────────────────

DATA_DIR="data"
BACKUP_DIR="backups/bbolt"
KEEP_COUNT=7

AGENTS_DB="${DATA_DIR}/agents.db"
CONTENT_DB="${DATA_DIR}/content_store.db"
BLOBS_DIR="${DATA_DIR}/content_store_blobs"

TS="$(date +%Y%m%d-%H%M%S)"
OUTFILE="${BACKUP_DIR}/cambrian-bbolt-${TS}.tar.gz"

# ── Helpers ─────────────────────────────────────────────────────────────────

die() {
  printf '%s\n' "$1" >&2
  exit 1
}

warn() {
  printf '%s\n' "$1" >&2
}

# ── Preconditions ───────────────────────────────────────────────────────────

mkdir -p "${BACKUP_DIR}"

if [ ! -f "${AGENTS_DB}" ]; then
  die "ERROR: BBolt database not found: ${AGENTS_DB}"
fi

if [ ! -f "${CONTENT_DB}" ]; then
  die "ERROR: BBolt content-store database not found: ${CONTENT_DB}"
fi

# ── bbolt availability ──────────────────────────────────────────────────────

BBOLT_CMD=""

if command -v bbolt >/dev/null 2>&1; then
  BBOLT_CMD="bbolt"
  printf 'Using bbolt CLI for consistent online backup.\n'
else
  # Try go run as a secondary option (requires Go toolchain and module deps)
  if command -v go >/dev/null 2>&1; then
    if go run go.etcd.io/bbolt/cmd/bbolt version >/dev/null 2>&1; then
      BBOLT_CMD="go run go.etcd.io/bbolt/cmd/bbolt"
      printf 'Using "go run" bbolt for consistent online backup.\n'
    fi
  fi
fi

if [ -z "${BBOLT_CMD}" ]; then
  warn '============================================================'
  warn 'WARNING: bbolt CLI is not available.'
  warn '         Falling back to plain "cp" for the database files.'
  warn ''
  warn '         The Cambrian runtime MUST be stopped before this'
  warn '         backup runs, or the copied files may be corrupt.'
  warn '         BBolt is single-writer; copying an open database'
  warn '         is unsafe.'
  warn '============================================================'
fi

# ── Stage and archive ───────────────────────────────────────────────────────

TMPDIR="$(mktemp -d)"
trap 'rm -rf "${TMPDIR}"' EXIT

mkdir -p "${TMPDIR}/content_store_blobs"

# Backup agents.db
if [ -n "${BBOLT_CMD}" ]; then
  ${BBOLT_CMD} backup "${AGENTS_DB}" "${TMPDIR}/agents.db"
else
  cp "${AGENTS_DB}" "${TMPDIR}/agents.db"
fi

# Backup content_store.db
if [ -n "${BBOLT_CMD}" ]; then
  ${BBOLT_CMD} backup "${CONTENT_DB}" "${TMPDIR}/content_store.db"
else
  cp "${CONTENT_DB}" "${TMPDIR}/content_store.db"
fi

# Copy blobs (filesystem directory; safe to copy while running)
if [ -d "${BLOBS_DIR}" ]; then
  cp -r "${BLOBS_DIR}"/* "${TMPDIR}/content_store_blobs/" 2>/dev/null || true
fi

# Create tarball
tar czf "${OUTFILE}" -C "${TMPDIR}" .

# ── Verify ──────────────────────────────────────────────────────────────────

if [ ! -s "${OUTFILE}" ]; then
  die "ERROR: Backup tarball is empty or missing."
fi

SIZE="$(du -h "${OUTFILE}" | cut -f1)"
printf 'Backup complete: %s (%s)\n' "${OUTFILE}" "${SIZE}"

# ── Rotation ────────────────────────────────────────────────────────────────

COUNT="$(find "${BACKUP_DIR}" -maxdepth 1 -name 'cambrian-bbolt-*.tar.gz' -type f | sort | wc -l)"
if [ "${COUNT}" -gt "${KEEP_COUNT}" ]; then
  find "${BACKUP_DIR}" -maxdepth 1 -name 'cambrian-bbolt-*.tar.gz' -type f | sort | head -n -"${KEEP_COUNT}" | while IFS= read -r f; do
    printf 'Rotating out old backup: %s\n' "${f}"
    rm -f "${f}"
  done
fi

printf 'Done. %d backup(s) retained in %s.\n' "${KEEP_COUNT}" "${BACKUP_DIR}"
