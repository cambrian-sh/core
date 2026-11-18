# Backup, restore and the restore drill

Backups are **deployment-owned**: the kernel binary takes, verifies, rotates, restores and
drills them; the host schedules them. (Owner decision 2026-11-15, ticket ENG-681. An
operator-plane backup surface may come later.)

## What is backed up

| Item | How | Where |
| --- | --- | --- |
| The kernel database (`database.dbname`, Postgres + pgvector) | `pg_dump -Fc`, with exact per-table row counts taken at the same snapshot | `<backup.dir>/<run>/db.dump` |
| The config store `configs/config.db` (ADR-0101) | consistent bbolt copy | `<run>/files/config.db` |
| The storage data directory (`storage.data_dir`: the reactive journal `agents.db`, the content store and its blobs, vault, outputs) | bbolt files copied consistently, other files byte for byte | `<run>/files/data/…` |
| `secret.key` (decrypts every secret in `config.db`) | **encrypted** with AES-256-GCM, key derived from a passphrase (PBKDF2-HMAC-SHA256) | `<backup.key_dir>/<run>/secret.key.enc` — never inside the run folder |

Not backed up: Langfuse (has its own store), benchmark databases (`cambrian_bench`,
`cambrian_smoke`, …). Each run folder ends with `manifest.json` (kind, database, schema
version, row counts, every file's size and sha256), written last: a folder without one is
an incomplete run that `verify` refuses.

## Configuration (`backup` block)

```json
"backup": {
  "dir": "backups",
  "key_dir": "",
  "passphrase_env": "CAMBRIAN_BACKUP_PASSPHRASE",
  "pg_container": "",
  "drill_dbname": "",
  "retention": { "daily": 7, "weekly": 4, "monthly": 3 }
}
```

- `dir` — root of the run folders; relative paths resolve against the kernel's base directory.
- `key_dir` — root for the encrypted `secret.key` copies; empty means the sibling
  `<dir>-keys`. It may not be inside `dir`.
- `passphrase_env` — the **name** of the environment variable holding the passphrase. The
  passphrase is never a config value. Without it, `backup run` refuses while `secret.key`
  exists (or pass `--no-secret-key`). Keep the passphrase somewhere other than this machine:
  without it the encrypted key cannot be recovered, and without the key the stored secrets
  must be re-entered.
- `pg_container` — run `pg_dump`/`pg_restore` inside this Docker container (e.g.
  `cambrian-db`); empty means the PostgreSQL client tools from `PATH`, which must be the
  server's major version or newer. The password is passed in `PGPASSWORD`, never on a
  command line. `orchestrator setup` sets it to `cambrian-db` when it manages the bundled
  Postgres (an existing value is kept); the kernel images carry the PostgreSQL 16 client
  tools, so a containerised kernel needs no `pg_container`.
- `drill_dbname` — the drill's scratch database; empty means `<dbname>_restore_drill`. It
  can never be the live database or a protected one.
- `retention` — keep the newest run of each of the last 7 days, 4 ISO weeks and 3 months
  (UTC). Pre-migration and pre-restore snapshots are never rotated away; remove them by hand.

Every key can be set by environment, e.g. `CAMBRIAN_BACKUP__DIR=/var/backups/cambrian`.

Backups live on the same machine for now; an off-machine copy is a separate, deferred
decision (ticket T-20261115-kernel-backups-stay-on-the-same-machine-as-the). The backup
directories are hidden from sandboxed MCP servers by default (`mcp.sandbox.hide_paths`).

## Commands

```sh
orchestrator backup run     [--kind nightly|manual] [--no-secret-key] [--no-prune]
orchestrator backup verify  [RUN]
orchestrator backup list
orchestrator backup prune
orchestrator backup restore RUN --to-db NAME [--replace-live] [--skip-snapshot] [--files-to DIR] [--secret-key-to PATH]
orchestrator backup drill   [RUN] [--keep]
```

`RUN` is a run folder, its name, or `latest`. Every verb accepts `--dir <kernel-dir>`. The
premium binary has the same verbs.

- **restore** loads the dump into a **new** database and checks every table's row count
  against the manifest. Over the live (or a protected) database only with `--replace-live`;
  a `pre-restore` snapshot of the live database is taken first unless `--skip-snapshot`.
  `--files-to` extracts `config.db` and `data/` into a new directory (stop the kernel, then
  move them into place); `--secret-key-to` decrypts `secret.key` to a path that must not exist.
- **drill** restores a run into the scratch database, compares every table's row count with
  the manifest, and drops the scratch database. It drops an existing scratch database only
  if the drill created it (database comment marker). Exit code 1 on any mismatch.

## Mandatory snapshot before a destructive migration

`migrate up` and the boot-time migration runner take a database snapshot (kind
`pre-migration`, under `backup.dir`) before applying any migration marked destructive to a
database that already has applied migrations, and **refuse** — writing nothing — if the
snapshot fails. A fresh database needs no snapshot.

- A migration is destructive when its file carries the line `-- cambrian:destructive`.
  Migrations shipped before the marker existed (0001–0050) are classified in
  `internal/migrate/destructive.go` (0007, 0017, 0021, 0028, 0045, 0047 are destructive). A
  test fails for any migration that contains `DROP TABLE`, `DROP COLUMN`, `DROP SCHEMA`,
  `TRUNCATE`, `DELETE FROM` or `ALTER … TYPE` without `-- cambrian:destructive` or
  `-- cambrian:not-destructive: <reason>`.
- Operator override, for one command only: `orchestrator migrate up --skip-snapshot`, or
  `CAMBRIAN_DATABASE__MIGRATIONS__ALLOW_DESTRUCTIVE_WITHOUT_SNAPSHOT=true`. Use it only when
  you hold another restorable copy.

## Scheduling (not installed automatically)

Run `backup run` nightly and `backup drill` weekly, with the passphrase in the scheduler's
environment. The examples assume the kernel is installed in `C:\cambrian` / `/opt/cambrian`.

### Windows Task Scheduler

Store the passphrase as a user environment variable of the account the task runs as
(`setx CAMBRIAN_BACKUP_PASSPHRASE "<passphrase>"` in that account), then:

```bat
schtasks /Create /TN "Cambrian nightly backup" /SC DAILY /ST 03:00 /RU "%USERNAME%" ^
  /TR "\"C:\cambrian\bin\orchestrator.exe\" backup run --kind nightly --dir C:\cambrian"
schtasks /Create /TN "Cambrian weekly restore drill" /SC WEEKLY /D SUN /ST 04:30 /RU "%USERNAME%" ^
  /TR "\"C:\cambrian\bin\orchestrator.exe\" backup drill --dir C:\cambrian"
```

Check results with `schtasks /Query /TN "Cambrian nightly backup" /V /FO LIST` (Last Result 0 = success).

### Linux systemd timers

`/etc/cambrian/backup.env` (mode 0600): `CAMBRIAN_BACKUP_PASSPHRASE=<passphrase>`

```ini
# /etc/systemd/system/cambrian-backup.service
[Unit]
Description=Cambrian nightly backup
[Service]
Type=oneshot
User=cambrian
EnvironmentFile=/etc/cambrian/backup.env
ExecStart=/opt/cambrian/bin/orchestrator backup run --kind nightly --dir /opt/cambrian

# /etc/systemd/system/cambrian-backup.timer
[Unit]
Description=Cambrian nightly backup
[Timer]
OnCalendar=*-*-* 03:00:00
Persistent=true
[Install]
WantedBy=timers.target
```

The drill is the same pair with `ExecStart=… backup drill --dir /opt/cambrian` and
`OnCalendar=Sun *-*-* 04:30:00`. Enable with `systemctl enable --now cambrian-backup.timer
cambrian-drill.timer`; failures show in `systemctl list-timers` and `journalctl -u cambrian-backup`.

### cron

```cron
0 3 * * *   . /etc/cambrian/backup.env; /opt/cambrian/bin/orchestrator backup run --kind nightly --dir /opt/cambrian >> /var/log/cambrian-backup.log 2>&1
30 4 * * 0  . /etc/cambrian/backup.env; /opt/cambrian/bin/orchestrator backup drill --dir /opt/cambrian >> /var/log/cambrian-backup.log 2>&1
```

(`/etc/cambrian/backup.env` must then use `export CAMBRIAN_BACKUP_PASSPHRASE=…`.)

## Production option: WAL archiving and point-in-time recovery (documented, not built)

`pg_dump` gives a consistent logical copy per run: you can lose up to a day of writes, and a
dump of large pgvector/HNSW tables restores by rebuilding the indexes. For production, add
continuous WAL archiving with [wal-g](https://github.com/wal-g/wal-g): set
`archive_mode = on` and `archive_command = 'wal-g wal-push %p'`, take a nightly
`wal-g backup-push $PGDATA`, and recover to any moment with `wal-g backup-fetch` plus
`restore_command = 'wal-g wal-fetch %f %p'` and `recovery_target_time`. Physical backups copy
index files as they are (no rebuild) and need the same Postgres major version and pgvector
build to restore. Keep the logical runs above as well: they are what the drill checks and
what the pre-migration guard takes.
