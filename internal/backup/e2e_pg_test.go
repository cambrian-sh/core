package backup_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/internal/backup"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/migrate"
	"github.com/cambrian-sh/core/internal/storage"
)

// End-to-end through the real path (ENG-681): a real Postgres database is migrated and
// seeded, backed up with pg_dump, verified, restored into a scratch database by the drill
// and into a new database by restore, with every table's row count checked; the
// destructive-migration guard is driven through postgres.RunMigrations (the `migrate up`
// path) with a working and a broken snapshot.
//
// It needs PG_TEST_DSN (a server and a role allowed to CREATE DATABASE; the database in
// the DSN is not touched) and pg_dump/pg_restore — from PATH, or inside the container
// named by PG_TEST_CONTAINER. It creates its own uniquely named databases and drops only those.

type pgEnv struct {
	admin  *pgx.ConnConfig
	pg     backup.PG
	prefix string
}

func newPgEnv(t *testing.T) *pgEnv {
	t.Helper()
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping the backup end-to-end test")
	}
	container := os.Getenv("PG_TEST_CONTAINER")
	if container == "" {
		if _, err := exec.LookPath("pg_dump"); err != nil {
			t.Skip("pg_dump not on PATH and PG_TEST_CONTAINER not set; skipping the backup end-to-end test")
		}
	}
	cc, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return &pgEnv{admin: cc, prefix: fmt.Sprintf("cambrian_backup_e2e_%d", time.Now().UnixNano()%1_000_000_000),
		pg: backup.PG{Host: cc.Host, Port: fmt.Sprint(cc.Port), User: cc.User, Password: cc.Password, Container: container}}
}

func (e *pgEnv) exec(t *testing.T, sql string) {
	t.Helper()
	c := e.admin.Copy()
	c.Database = "postgres"
	conn, err := pgx.ConnectConfig(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(context.Background(), sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// createDB creates a database this test owns and registers its drop.
func (e *pgEnv) createDB(t *testing.T, name string) {
	t.Helper()
	e.exec(t, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	e.ownDB(t, name)
}

// ownDB registers the drop of a database this test created (directly or via restore).
func (e *pgEnv) ownDB(t *testing.T, name string) {
	t.Cleanup(func() {
		if !strings.HasPrefix(name, e.prefix) {
			t.Errorf("refusing to drop %s: not created by this test", name)
			return
		}
		e.exec(t, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	})
}

func (e *pgEnv) pool(t *testing.T, db string) *pgxpool.Pool {
	t.Helper()
	c := e.admin.Copy()
	c.Database = db
	pc, err := pgxpool.ParseConfig("")
	if err != nil {
		t.Fatal(err)
	}
	pc.ConnConfig = c
	p, err := pgxpool.NewWithConfig(context.Background(), pc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

const e2eDim = 8

func TestBackupRestoreDrill_EndToEnd_Pg(t *testing.T) {
	ctx := context.Background()
	e := newPgEnv(t)
	live := e.prefix + "_live"
	e.createDB(t, live)
	pool := e.pool(t, live)
	if err := migrate.Migrate(ctx, pool, e2eDim); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, s := range []string{
		`CREATE TABLE e2e_items (id int PRIMARY KEY, body text, embedding vector(3))`,
		`INSERT INTO e2e_items SELECT g, 'row ' || g, ARRAY[g, g+1, g+2]::real[]::vector FROM generate_series(1, 500) g`,
	} {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// Kernel-local state: a config store (which creates secret.key beside it) and a data dir.
	work := t.TempDir()
	storePath := filepath.Join(work, "configs", "config.db")
	if err := os.MkdirAll(filepath.Dir(storePath), 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := storage.OpenConfigStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetOverride("execution.retrieval.recall_top_k", 7); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(work, "configs", "secret.key")
	keyHex, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("config store did not create secret.key: %v", err)
	}
	dataDir := filepath.Join(work, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "outputs"), 0o700); err != nil {
		t.Fatal(err)
	}
	journal, err := storage.OpenConfigStore(filepath.Join(dataDir, "agents.db")) // any bbolt file stands in for the journal
	if err != nil {
		t.Fatal(err)
	}
	_ = journal.Close()
	_ = os.Remove(filepath.Join(dataDir, "secret.key")) // created beside it; not part of the data dir under test
	if err := os.WriteFile(filepath.Join(dataDir, "outputs", "report.txt"), []byte("artifact"), 0o600); err != nil {
		t.Fatal(err)
	}

	root, keyRoot := filepath.Join(work, "backups"), filepath.Join(work, "backups-keys")
	pg := e.pg.WithDB(live)
	dir, m, err := backup.Run(ctx, backup.Options{
		Root: root, KeyRoot: keyRoot, Kind: backup.KindManual, PG: pg,
		ConfigStore: storePath, DataDir: dataDir, SecretKeyPath: keyPath,
		Passphrase: []byte("e2e passphrase"), PassphraseEnv: "CAMBRIAN_BACKUP_PASSPHRASE",
	})
	if err != nil {
		t.Fatalf("backup run: %v", err)
	}

	// Manifest: exact counts at the dump's snapshot.
	counts := map[string]int64{}
	for _, tc := range m.Tables {
		counts[tc.Table] = tc.Rows
	}
	if counts["public.e2e_items"] != 500 {
		t.Fatalf("manifest e2e_items = %d, want 500", counts["public.e2e_items"])
	}
	if m.SchemaVersion < 45 || m.Database != live {
		t.Fatalf("manifest database=%s schema=%d", m.Database, m.SchemaVersion)
	}
	kinds := map[string]bool{}
	for _, f := range m.Files {
		kinds[f.Name] = true
	}
	for _, want := range []string{"db.dump", "files/config.db", "files/data/agents.db", "files/data/outputs/report.txt"} {
		if !kinds[want] {
			t.Fatalf("manifest lacks %s: %+v", want, m.Files)
		}
	}

	// secret.key: never in plaintext anywhere in the run folder; encrypted in the key root.
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), strings.TrimSpace(string(keyHex))) || filepath.Base(p) == "secret.key" {
			t.Errorf("plaintext secret.key material found in the backup folder: %s", p)
		}
		return nil
	})
	if m.SecretKey == nil || m.SecretKey.Status != "encrypted" || !strings.HasPrefix(m.SecretKey.Path, keyRoot) {
		t.Fatalf("secret key ref: %+v", m.SecretKey)
	}
	enc, _ := os.ReadFile(m.SecretKey.Path)
	if strings.Contains(string(enc), strings.TrimSpace(string(keyHex))) {
		t.Fatal("the encrypted copy contains the plaintext key")
	}
	back := filepath.Join(work, "restored-secret.key")
	if err := backup.DecryptFile(m.SecretKey.Path, back, []byte("e2e passphrase")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(back); string(b) != string(keyHex) {
		t.Fatal("decrypted secret.key differs from the original")
	}

	// Verify, including pg_restore --list.
	if _, err := backup.Verify(ctx, dir, &pg); err != nil {
		t.Fatalf("verify: %v", err)
	}

	// Drill into a scratch database: row counts match, scratch dropped.
	scratch := e.prefix + "_restore_drill"
	e.ownDB(t, scratch)
	res, err := backup.Drill(ctx, dir, pg, scratch, backup.RestoreOptions{Live: live}, false)
	if err != nil {
		t.Fatalf("drill: %v", err)
	}
	if len(res.Mismatches) != 0 || res.Rows < 500 {
		t.Fatalf("drill mismatches %v rows %d", res.Mismatches, res.Rows)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM pg_database WHERE datname = $1`, scratch).Scan(&n)
	if n != 0 {
		t.Fatal("drill left its scratch database behind")
	}
	// The drill refuses the live database and a foreign database without its marker.
	if _, err := backup.Drill(ctx, dir, pg, live, backup.RestoreOptions{Live: live}, false); err == nil {
		t.Fatal("drill into the live database must be refused")
	}
	foreign := e.prefix + "_foreign"
	e.createDB(t, foreign)
	if _, err := backup.Drill(ctx, dir, pg, foreign, backup.RestoreOptions{Live: live}, false); err == nil ||
		!strings.Contains(err.Error(), "not created by the drill") {
		t.Fatalf("drill over an unmarked database must be refused, got %v", err)
	}

	// Restore into a new database; over the live one only with --replace-live.
	target := e.prefix + "_restored"
	e.ownDB(t, target)
	_, mism, err := backup.RestoreDatabase(ctx, dir, pg, target, backup.RestoreOptions{Live: live})
	if err != nil || len(mism) != 0 {
		t.Fatalf("restore: %v %v", err, mism)
	}
	var vec string
	if err := e.pool(t, target).QueryRow(ctx, `SELECT embedding::text FROM e2e_items WHERE id = 7`).Scan(&vec); err != nil || vec != "[7,8,9]" {
		t.Fatalf("restored pgvector value %q, %v", vec, err)
	}
	if _, _, err := backup.RestoreDatabase(ctx, dir, pg, live, backup.RestoreOptions{Live: live}); err == nil {
		t.Fatal("restore over the live database without --replace-live must be refused")
	}
	if _, _, err := backup.RestoreDatabase(ctx, dir, pg, foreign, backup.RestoreOptions{Live: live}); err == nil {
		t.Fatal("restore over an existing non-live database must be refused")
	}
	// Files extract into a new directory only.
	got, err := backup.ExtractFiles(dir, m, filepath.Join(work, "extracted"))
	if err != nil || got != 3 {
		t.Fatalf("extract: %d %v", got, err)
	}
}

func TestMigrateUp_DestructiveMigrationSnapshotGuard_EndToEnd_Pg(t *testing.T) {
	ctx := context.Background()
	e := newPgEnv(t)
	live := e.prefix + "_mig"
	e.createDB(t, live)
	pool := e.pool(t, live)
	if err := migrate.Migrate(ctx, pool, e2eDim); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	has45 := func() bool {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version = 45`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	// Make 0045_delete_neural_traces (destructive, idempotent) pending again on a database with history.
	unapply := func() {
		if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = 45`); err != nil {
			t.Fatal(err)
		}
	}

	cfg := config.DefaultConfig()
	cfg.Database.Host, cfg.Database.Port = e.pg.Host, e.pg.Port
	cfg.Database.User, cfg.Database.Password, cfg.Database.DBName = e.pg.User, e.pg.Password, live
	cfg.Embedder.Dimensions = e2eDim
	work := t.TempDir()
	cfg.Backup.Dir = filepath.Join(work, "backups")
	cfg.Backup.KeyDir = filepath.Join(work, "keys")
	cfg.Backup.PGContainer = e.pg.Container

	// 1. Snapshot works: taken first, then the migration applies.
	unapply()
	if err := postgres.RunMigrations(ctx, cfg); err != nil {
		t.Fatalf("migrate up with a working snapshot: %v", err)
	}
	if !has45() {
		t.Fatal("0045 was not applied")
	}
	snap, err := backup.Latest(cfg.Backup.Dir, backup.KindPreMigration)
	if err != nil {
		t.Fatalf("no pre-migration snapshot: %v", err)
	}
	m, err := backup.Verify(ctx, snap, nil)
	if err != nil || !strings.Contains(m.Reason, "0045_delete_neural_traces") || m.SchemaVersion < 44 {
		t.Fatalf("snapshot manifest %+v, %v", m, err)
	}

	// 2. Snapshot cannot be taken: refused, nothing applied, no half folder left.
	unapply()
	cfg.Backup.PGContainer = e.prefix + "-no-such-container"
	before, _ := backup.ListRuns(cfg.Backup.Dir)
	err = postgres.RunMigrations(ctx, cfg)
	if !errors.Is(err, migrate.ErrSnapshotRequired) {
		t.Fatalf("want ErrSnapshotRequired, got %v", err)
	}
	if has45() {
		t.Fatal("a refused migrate up applied the destructive migration")
	}
	after, _ := backup.ListRuns(cfg.Backup.Dir)
	if len(after) != len(before) {
		t.Fatalf("a failed snapshot left a folder behind: %v", after)
	}

	// 3. Explicit operator override: applies without a snapshot.
	cfg.Database.Migrations.AllowDestructiveWithoutSnapshot = true
	if err := postgres.RunMigrations(ctx, cfg); err != nil {
		t.Fatalf("override: %v", err)
	}
	if !has45() {
		t.Fatal("override did not apply 0045")
	}
}
