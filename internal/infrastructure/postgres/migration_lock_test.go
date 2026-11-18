package postgres

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"testing/fstest"
	"time"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/migrate"
	"github.com/jackc/pgx/v5"
)

// The migration lock through the real paths (ENG-2029): kernels booting
// (NewPgVectorAdapter → migrate.Boot, then BootPluginMigrations → migrate.BootLanes, as
// bootstrapKernel does) race an operator's `migrate up` (RunMigrations +
// RunPluginMigrations) on one fresh scratch database. Every path succeeds and every
// core and plugin-lane migration lands in its ledger exactly once.

func TestMigrationPolicy_CarriesLockWait(t *testing.T) {
	cfg := &config.Config{}
	cfg.Database.Migrations.LockWaitSeconds = 42
	if got := migrationPolicy(cfg, migrate.ModeAuto).LockWait; got != 42*time.Second {
		t.Fatalf("Policy.LockWait = %s, want 42s from database.migrations.lock_wait_seconds", got)
	}
}

func TestPg_MigrationLock_KernelBootsRaceMigrateUp(t *testing.T) {
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping integration test that requires PostgreSQL")
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
	name := fmt.Sprintf("cambrian_test_miglock_e2e_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
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
	scratch := base.Copy()
	scratch.Database = name
	sc, err := pgx.ConnectConfig(ctx, scratch)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close(ctx)
	// The adapter's AfterConnect registers the vector type, so the extension must exist
	// before it connects (the same precondition the boot-guard test sets up).
	if _, err := sc.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.Database.Host = base.Host
	cfg.Database.Port = strconv.Itoa(int(base.Port))
	cfg.Database.User = base.User
	cfg.Database.Password = base.Password
	cfg.Database.DBName = name
	cfg.Embedder.Dimensions = 4
	cfg.Storage.AutoMigrate = true
	cfg.Database.Migrations.Mode = "auto"
	cfg.Database.Migrations.LockWaitSeconds = 120
	cfg.Backup.Dir = t.TempDir()

	// Not idempotent: a second application of any file fails.
	lanes := []migrate.Lane{{Namespace: "e2elock", FS: fstest.MapFS{
		"0001_baseline.sql": {Data: []byte("CREATE TABLE e2e_lock_t (id TEXT PRIMARY KEY);")},
		"0002_column.sql":   {Data: []byte("ALTER TABLE e2e_lock_t ADD COLUMN note TEXT;")},
	}}}

	errs := make(chan error, 4)
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		go func() { // a kernel boot
			<-start
			a, err := NewPgVectorAdapter(ctx, cfg)
			if err != nil {
				errs <- fmt.Errorf("kernel boot: %w", err)
				return
			}
			defer a.Close()
			if err := a.BootPluginMigrations(ctx, lanes); err != nil {
				errs <- fmt.Errorf("kernel plugin lanes: %w", err)
				return
			}
			errs <- nil
		}()
		go func() { // `orchestrator migrate up`
			<-start
			if err := RunMigrations(ctx, cfg); err != nil {
				errs <- fmt.Errorf("migrate up: %w", err)
				return
			}
			if err := RunPluginMigrations(ctx, cfg, lanes); err != nil {
				errs <- fmt.Errorf("migrate up lanes: %w", err)
				return
			}
			errs <- nil
		}()
	}
	close(start)
	for i := 0; i < 4; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("a concurrent boot / migrate up failed: %v", err)
		}
	}

	recs, err := MigrationStatus(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := sc.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(recs) {
		t.Fatalf("schema_migrations has %d rows for %d known migrations", n, len(recs))
	}
	for _, r := range recs {
		if r.Pending {
			t.Fatalf("migration %04d_%s still pending after the race", r.Version, r.Name)
		}
	}
	lrecs, err := PluginMigrationStatus(ctx, cfg, lanes)
	if err != nil || len(lrecs) != 2 || lrecs[0].Pending || lrecs[1].Pending {
		t.Fatalf("plugin lane status: %+v %v", lrecs, err)
	}
	var got bool
	if err := sc.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, migrate.MigrationLockKey).Scan(&got); err != nil || !got {
		t.Fatalf("the migration lock is still held after every path returned (got=%v err=%v)", got, err)
	}
}
