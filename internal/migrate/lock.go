package migrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The migration lock (T-20261118-migration-runner-takes-no-advisory-lock-two, ENG-2029).
//
// Every migration is its own transaction followed by a ledger INSERT, so two binaries
// that start on one database with a pending migration (two replicas, or a kernel and
// `orchestrator migrate up`) would both run it: the second fails on the ledger primary
// key, or mid-file for SQL that is not idempotent, and its boot fails. Since ENG-1457
// the runner is the only schema writer (premium's stores run no DDL), so the runner is
// where the exclusion belongs.
//
// Every APPLY entry point — Boot, BootLanes, Migrate, MigrateLanes — takes one Postgres
// session advisory lock before its first read and holds it to its last write. Reading
// under the lock is the point: the policy checks, the destructive-migration snapshot
// decision (ENG-681) and the applied-version sets are all read after the previous
// holder finished, so a second boot sees nothing pending, takes no second snapshot and
// applies nothing. Status / StatusLanes take no lock (reads only).
//
// One key for core AND every plugin lane: a lane's tables may reference core's, so a
// core chain and a lane chain must not interleave either. Advisory locks are scoped to
// the current database, so binaries on other databases of the same server never wait.
//
// The lock lives on a dedicated connection (a copy of the pool's connection config),
// not a pool connection: its lifetime must not depend on the pool's size (a pool of one
// would deadlock against the migration's own queries), and closing that connection
// always releases it. A holder that crashes loses its session and Postgres frees the
// lock — the reason for a session lock over a lock row in a table.
//
// Rejected: a transaction-scoped lock (pg_advisory_xact_lock) — each migration commits
// separately and the concurrent index builds run outside any transaction; blocking
// pg_advisory_lock — it cannot say it is waiting or who holds the lock, and
// cancellation would rest on pgx's out-of-band cancel request.

// MigrationLockKey is the advisory lock key every migration entry point takes: the
// ASCII bytes of "cambrian" as a big-endian int64. Any tool that changes a Cambrian
// schema outside this runner should take it too (SELECT pg_advisory_lock(7161125152089530734)).
const MigrationLockKey int64 = 0x63616d627269616e

// DefaultLockWait is how long a boot or `migrate up` waits for another binary's
// migration to finish when Policy.LockWait is zero (database.migrations.lock_wait_seconds).
// Generous on purpose: the holder may be building an index concurrently on a large
// table; a waiter that gives up fails its boot, which a supervisor restarts.
const DefaultLockWait = 15 * time.Minute

// ErrLockTimeout is returned (wrapped) when the migration lock is still held by another
// session after the wait limit.
var ErrLockTimeout = errors.New("timed out waiting for the migration lock")

// Poll and progress-log intervals; variables so tests can shorten them.
var (
	lockPollInterval    = 250 * time.Millisecond
	lockLogInterval     = 30 * time.Second
	lockReleaseDeadline = 10 * time.Second
)

// withMigrationLock runs fn while this process holds the migration lock on the pool's
// database. wait <= 0 means DefaultLockWait. The lock is released on every exit path,
// including a panic in fn and a cancelled ctx.
func withMigrationLock(ctx context.Context, pool *pgxpool.Pool, wait time.Duration, fn func() error) error {
	release, err := acquireMigrationLock(ctx, pool, wait)
	if err != nil {
		return err
	}
	defer release()
	return fn()
}

// acquireMigrationLock takes the session advisory lock on a dedicated connection and
// returns the function that releases it.
func acquireMigrationLock(ctx context.Context, pool *pgxpool.Pool, wait time.Duration) (func(), error) {
	if pool == nil {
		return nil, errors.New("migrate: nil pool")
	}
	return acquireMigrationLockConfig(ctx, pool.Config().ConnConfig.Copy(), wait)
}

// acquireMigrationLockConfig is acquireMigrationLock for a caller that has no pool yet
// (EnsureVectorExtension runs before the kernel's pool exists): the lock connection
// is opened from cc, which the caller must not share.
func acquireMigrationLockConfig(ctx context.Context, cc *pgx.ConnConfig, wait time.Duration) (func(), error) {
	if wait <= 0 {
		wait = DefaultLockWait
	}
	conn, err := pgx.ConnectConfig(ctx, cc)
	if err != nil {
		return nil, fmt.Errorf("migrate: open the migration-lock connection: %w", err)
	}
	closeConn := func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lockReleaseDeadline)
		defer cancel()
		_ = conn.Close(cctx) // best effort: a closed session holds no advisory lock
	}

	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	started := time.Now()
	var lastLog time.Time
	for {
		var got bool
		err := conn.QueryRow(waitCtx, `SELECT pg_try_advisory_lock($1)`, MigrationLockKey).Scan(&got)
		if err == nil && got {
			if !lastLog.IsZero() {
				slog.Warn("migrate: migration lock acquired after waiting", "database", conn.Config().Database,
					"waited", time.Since(started).Round(time.Millisecond).String())
			}
			break
		}
		if err != nil && waitCtx.Err() == nil {
			closeConn()
			return nil, fmt.Errorf("migrate: try the migration lock: %w", err)
		}
		if err == nil && (lastLog.IsZero() || time.Since(lastLog) >= lockLogInterval) {
			// WARN: a boot that sits here is not hung, it is queued behind another binary.
			slog.Warn("migrate: waiting for the migration lock — another binary is migrating this database",
				"database", conn.Config().Database, "holder", lockHolder(waitCtx, conn),
				"waited", time.Since(started).Round(time.Second).String(), "wait_limit", wait.String(),
				"lock_key", MigrationLockKey)
			lastLog = time.Now()
		}
		select {
		case <-waitCtx.Done():
		case <-time.After(lockPollInterval):
			continue
		}
		holder := lockHolder(context.WithoutCancel(ctx), conn)
		closeConn()
		if ctx.Err() != nil {
			return nil, fmt.Errorf("migrate: gave up waiting for the migration lock (held by %s): %w", holder, ctx.Err())
		}
		return nil, fmt.Errorf("%w: database %q, held by %s for longer than %s. Nothing was written. "+
			"Another binary is applying migrations to this database; let it finish and boot again, "+
			"or raise database.migrations.lock_wait_seconds (CAMBRIAN_DATABASE__MIGRATIONS__LOCK_WAIT_SECONDS)",
			ErrLockTimeout, conn.Config().Database, holder, wait)
	}

	return func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lockReleaseDeadline)
		defer cancel()
		var ok bool
		if err := conn.QueryRow(rctx, `SELECT pg_advisory_unlock($1)`, MigrationLockKey).Scan(&ok); err != nil || !ok {
			// Closing the connection below ends the session, which releases the lock anyway.
			slog.Warn("migrate: explicit migration-lock unlock failed; closing its session releases it",
				"database", conn.Config().Database, "err", err, "unlocked", ok)
		}
		_ = conn.Close(rctx) // best effort: see above
	}, nil
}

// lockHolder describes the session holding the migration lock, for the wait log and
// the timeout error. Best effort: an empty or failed lookup reads "unknown".
func lockHolder(ctx context.Context, conn *pgx.Conn) string {
	qctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var pid int32
	var app, addr string
	// A bigint advisory key shows in pg_locks as classid = high 32 bits, objid = low
	// 32 bits, objsubid = 1.
	err := conn.QueryRow(qctx, `
		SELECT l.pid, COALESCE(a.application_name, ''), COALESCE(host(a.client_addr), 'local')
		FROM pg_locks l LEFT JOIN pg_stat_activity a ON a.pid = l.pid
		WHERE l.locktype = 'advisory' AND l.granted AND l.objsubid = 1
		  AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND l.classid::bigint = ($1::bigint >> 32)
		  AND l.objid::bigint = ($1::bigint & 4294967295)
		LIMIT 1`, MigrationLockKey).Scan(&pid, &app, &addr)
	if err != nil {
		return "unknown session"
	}
	if app == "" {
		app = "-"
	}
	return fmt.Sprintf("pid %d (application %s, client %s)", pid, app, addr)
}
