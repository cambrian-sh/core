package migrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The migration lock (lock.go, ENG-2029).

func TestMigrationLockKey_IsCambrianASCII(t *testing.T) {
	var b [8]byte
	for i := range b {
		b[i] = byte(uint64(MigrationLockKey) >> (56 - 8*i))
	}
	if string(b[:]) != "cambrian" {
		t.Fatalf("MigrationLockKey bytes = %q; the documented key is ASCII \"cambrian\"", b)
	}
}

func TestAcquireMigrationLock_NilPool(t *testing.T) {
	if _, err := acquireMigrationLock(context.Background(), nil, time.Second); err == nil {
		t.Fatal("a nil pool must be refused, not panic")
	}
}

// logCapture records every slog message while a test runs (the runner logs through the
// default logger). Tests in this package do not run in parallel.
type logCapture struct {
	mu   sync.Mutex
	msgs []string
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, r.Message)
	return nil
}
func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *logCapture) WithGroup(string) slog.Handler      { return c }

func (c *logCapture) count(prefix string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, m := range c.msgs {
		if strings.HasPrefix(m, prefix) {
			n++
		}
	}
	return n
}

func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	c := &logCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(c))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return c
}

func fastLockPoll(t *testing.T) {
	t.Helper()
	prev := lockPollInterval
	lockPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { lockPollInterval = prev })
}

// scratchDB creates a uniquely named database on PG_TEST_DSN's server (dropped in
// cleanup, WITH (FORCE)) and returns a pool factory for it: each call is a new pool,
// standing in for a separate binary.
func scratchDB(t *testing.T) (context.Context, func() *pgxpool.Pool, string) {
	t.Helper()
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set")
	}
	ctx := context.Background()
	base, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgx.ConnectConfig(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	name := fmt.Sprintf("cambrian_test_miglock_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	var pools []*pgxpool.Pool
	var mu sync.Mutex
	t.Cleanup(func() {
		mu.Lock()
		for _, p := range pools {
			p.Close()
		}
		mu.Unlock()
		c, err := pgx.ConnectConfig(context.Background(), base)
		if err != nil {
			t.Logf("drop %s: %v", name, err)
			return
		}
		defer c.Close(context.Background())
		if _, err := c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("drop %s: %v", name, err)
		}
	})
	open := func() *pgxpool.Pool {
		cfg, err := pgxpool.ParseConfig("")
		if err != nil {
			t.Fatal(err)
		}
		cfg.ConnConfig.Host, cfg.ConnConfig.Port = base.Host, base.Port
		cfg.ConnConfig.User, cfg.ConnConfig.Password = base.User, base.Password
		cfg.ConnConfig.TLSConfig, cfg.ConnConfig.Database = base.TLSConfig, name
		p, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		pools = append(pools, p)
		mu.Unlock()
		return p
	}
	return ctx, open, name
}

func countRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// lockFree reports whether nobody holds the migration lock on pool's database: it
// takes and drops it.
func lockFree(ctx context.Context, t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, MigrationLockKey).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got {
		if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, MigrationLockKey); err != nil {
			t.Fatal(err)
		}
	}
	return got
}

// nonIdempotentLane fails on a second application of any file (no IF NOT EXISTS), so a
// double apply cannot hide.
var nonIdempotentLane = Lane{Namespace: "locklane", FS: laneFS(map[string]string{
	"0001_baseline.sql": "CREATE TABLE lock_lane_t (id TEXT PRIMARY KEY);",
	"0002_column.sql":   "ALTER TABLE lock_lane_t ADD COLUMN note TEXT;",
	"0003_index.sql":    "CREATE INDEX lock_lane_t_note ON lock_lane_t (note);",
})}

// Three binaries boot (core chain, then the plugin lane, as bootstrapKernel does) on
// one fresh database at once: all succeed, every core and lane migration is applied
// exactly once, and the ones that waited applied nothing.
func TestPg_MigrationLock_ConcurrentBootsApplyEachMigrationOnce(t *testing.T) {
	ctx, open, _ := scratchDB(t)
	logs := captureLogs(t)
	fastLockPoll(t)
	migs, err := loadMigrations(splitTestDim)
	if err != nil {
		t.Fatal(err)
	}

	const binaries = 3
	errs := make(chan error, binaries)
	start := make(chan struct{})
	for i := 0; i < binaries; i++ {
		pool := open()
		go func() {
			<-start
			if err := Boot(ctx, pool, splitTestDim, Policy{Mode: ModeAuto}); err != nil {
				errs <- fmt.Errorf("core: %w", err)
				return
			}
			if err := BootLanes(ctx, pool, Policy{Mode: ModeAuto}, []Lane{nonIdempotentLane}); err != nil {
				errs <- fmt.Errorf("lane: %w", err)
				return
			}
			errs <- nil
		}()
	}
	close(start)
	for i := 0; i < binaries; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("a concurrent boot failed: %v", err)
		}
	}

	pool := open()
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM schema_migrations`); n != len(migs) {
		t.Fatalf("schema_migrations has %d rows, want %d", n, len(migs))
	}
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM plugin_schema_migrations WHERE namespace = 'locklane'`); n != 3 {
		t.Fatalf("plugin_schema_migrations has %d locklane rows, want 3", n)
	}
	if n := logs.count("migrate: applying schema migration"); n != len(migs) {
		t.Fatalf("core migrations were applied %d times in total, want exactly %d (once each)", n, len(migs))
	}
	if n := logs.count("migrate: applying plugin schema migration"); n != 3 {
		t.Fatalf("lane migrations were applied %d times in total, want exactly 3", n)
	}
	if n := logs.count("migrate: waiting for the migration lock"); n == 0 {
		t.Log("no boot had to wait (the first finished before the others started); exactly-once still held")
	}
	pending, err := pendingChanges(ctx, pool, splitTestDim)
	if err != nil || len(pending) != 0 {
		t.Fatalf("after the boots: pending %v, err %v", pending, err)
	}
	if !lockFree(ctx, t, pool) {
		t.Fatal("the migration lock is still held after every boot returned")
	}
}

// A pending DESTRUCTIVE lane migration on a database with history: two binaries boot
// at once; the snapshot is taken exactly once (by the lock holder), the waiter re-reads
// under the lock, finds nothing pending and neither snapshots nor applies.
func TestPg_MigrationLock_SnapshotGuardRunsOnce(t *testing.T) {
	ctx, open, _ := scratchDB(t)
	logs := captureLogs(t)
	fastLockPoll(t)
	v1 := Lane{Namespace: "locksnap", FS: laneFS(map[string]string{"0001_t.sql": "CREATE TABLE lock_snap_t (id int);"})}
	if err := MigrateLanes(ctx, open(), []Lane{v1}); err != nil {
		t.Fatal(err)
	}
	v2 := Lane{Namespace: "locksnap", FS: laneFS(map[string]string{
		"0001_t.sql":    "CREATE TABLE lock_snap_t (id int);",
		"0002_drop.sql": "-- cambrian:destructive\nDROP TABLE lock_snap_t;",
	})}

	appliedBefore := logs.count("migrate: applying plugin schema migration") // v1's 0001
	var snapshots atomic.Int32
	entered := make(chan struct{}, 2)
	proceed := make(chan struct{})
	snap := func(context.Context, string, []string) (string, error) {
		snapshots.Add(1)
		entered <- struct{}{}
		<-proceed
		return "snap", nil
	}
	errs := make(chan error, 2)
	boot := func() { errs <- BootLanes(ctx, open(), Policy{Mode: ModeAuto, Snapshot: snap}, []Lane{v2}) }

	go boot()
	<-entered // the first binary holds the lock and is snapshotting
	go boot()
	deadline := time.Now().Add(10 * time.Second)
	for logs.count("migrate: waiting for the migration lock") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the second boot never reported that it is waiting for the migration lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(proceed)
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("boot %d: %v", i, err)
		}
	}
	if n := snapshots.Load(); n != 1 {
		t.Fatalf("snapshot taken %d times, want exactly 1", n)
	}
	pool := open()
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM plugin_schema_migrations WHERE namespace = 'locksnap'`); n != 2 {
		t.Fatalf("locksnap ledger rows = %d, want 2", n)
	}
	if n := logs.count("migrate: applying plugin schema migration") - appliedBefore; n != 1 {
		t.Fatalf("0002_drop applied %d times, want 1", n)
	}
}

// A lock holder whose context is cancelled mid-migration releases the lock: the
// binary queued behind it acquires it, and migrates.
func TestPg_MigrationLock_CancelledHolderReleases(t *testing.T) {
	ctx, open, _ := scratchDB(t)
	fastLockPoll(t)
	logs := captureLogs(t)
	v1 := Lane{Namespace: "lockcancel", FS: laneFS(map[string]string{"0001_t.sql": "CREATE TABLE lock_cancel_t (id int);"})}
	if err := MigrateLanes(ctx, open(), []Lane{v1}); err != nil {
		t.Fatal(err)
	}
	v2 := Lane{Namespace: "lockcancel", FS: laneFS(map[string]string{
		"0001_t.sql":    "CREATE TABLE lock_cancel_t (id int);",
		"0002_drop.sql": "-- cambrian:destructive\nDROP TABLE lock_cancel_t;",
	})}

	holderCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	entered := make(chan struct{})
	blocking := func(c context.Context, _ string, _ []string) (string, error) {
		close(entered)
		<-c.Done()
		return "", c.Err()
	}
	holderErr := make(chan error, 1)
	go func() {
		holderErr <- BootLanes(holderCtx, open(), Policy{Mode: ModeAuto, Snapshot: blocking}, []Lane{v2})
	}()
	<-entered

	waiterErr := make(chan error, 1)
	var snapped atomic.Int32
	ok := func(context.Context, string, []string) (string, error) { snapped.Add(1); return "s", nil }
	go func() { waiterErr <- BootLanes(ctx, open(), Policy{Mode: ModeAuto, Snapshot: ok}, []Lane{v2}) }()
	deadline := time.Now().Add(10 * time.Second)
	for logs.count("migrate: waiting for the migration lock") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the second boot never queued behind the holder")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	// guardDestructive wraps the snapshot's error with %v, so the cancellation shows in
	// the text, not the chain.
	if err := <-holderErr; !errors.Is(err, ErrSnapshotRequired) || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("cancelled holder: want a snapshot refusal carrying the cancellation, got %v", err)
	}
	select {
	case err := <-waiterErr:
		if err != nil {
			t.Fatalf("the waiter failed after the holder was cancelled: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the waiter never acquired the lock: the cancelled holder did not release it")
	}
	if snapped.Load() != 1 {
		t.Fatalf("waiter snapshots = %d, want 1 (it found the destructive migration still pending)", snapped.Load())
	}
	pool := open()
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM plugin_schema_migrations WHERE namespace = 'lockcancel'`); n != 2 {
		t.Fatalf("lockcancel ledger rows = %d, want 2", n)
	}
	if !lockFree(ctx, t, pool) {
		t.Fatal("lock still held")
	}
}

// A boot that cannot get the lock gives up after Policy.LockWait with ErrLockTimeout
// naming the holder, or with the caller's context error when that ends first — and
// writes nothing either way. Another database on the same server is not blocked.
func TestPg_MigrationLock_WaitLimitAndCancellation(t *testing.T) {
	ctx, open, name := scratchDB(t)
	fastLockPoll(t)
	pool := open()
	holder, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_lock($1)`, MigrationLockKey); err != nil {
		t.Fatal(err)
	}
	var holderPID int32
	if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatal(err)
	}

	err = Boot(ctx, open(), splitTestDim, Policy{Mode: ModeAuto, LockWait: 300 * time.Millisecond})
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("want ErrLockTimeout, got %v", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("pid %d", holderPID)) || !strings.Contains(err.Error(), name) {
		t.Fatalf("the timeout must name the database and the holder's pid %d: %v", holderPID, err)
	}
	if relationExists(ctx, t, pool, "schema_migrations") {
		t.Fatal("a boot that never got the lock wrote schema_migrations")
	}

	cctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	err = BootLanes(cctx, open(), Policy{Mode: ModeAuto}, []Lane{nonIdempotentLane})
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrLockTimeout) {
		t.Fatalf("caller deadline while waiting: want context.DeadlineExceeded (not ErrLockTimeout), got %v", err)
	}
	if relationExists(ctx, t, pool, "plugin_schema_migrations") {
		t.Fatal("a lane boot that never got the lock wrote plugin_schema_migrations")
	}

	// Advisory locks are per database: a second scratch database migrates while the
	// first is locked.
	ctx2, open2, _ := scratchDB(t)
	if err := Boot(ctx2, open2(), splitTestDim, Policy{Mode: ModeAuto, LockWait: 5 * time.Second}); err != nil {
		t.Fatalf("another database must not wait for this one's lock: %v", err)
	}

	if _, err := holder.Exec(ctx, `SELECT pg_advisory_unlock($1)`, MigrationLockKey); err != nil {
		t.Fatal(err)
	}
	holder.Release()
	if err := Boot(ctx, open(), splitTestDim, Policy{Mode: ModeAuto, LockWait: 5 * time.Second}); err != nil {
		t.Fatalf("after the holder unlocked: %v", err)
	}
}
