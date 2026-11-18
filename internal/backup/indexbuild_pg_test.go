package backup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/internal/migrate"
)

// End-to-end for ENG-2123 through the real path: a store migrated with the real schema
// (chunks + idx_chunks_embedding_cosine, 1024 dims) is backed up with pg_dump and restored
// with pg_restore inside the database container. The container's /dev/shm is Docker's
// default 64 MB; the test's own login role carries maintenance_work_mem = 128MB (a role
// setting, so no server setting changes and nothing outside this test is affected), which
// makes a parallel HNSW rebuild ask for a ~128 MB shared memory segment. The negative
// control restores without the index build plan and must fail with the shared memory
// error the readiness drill hit; the drill (which plans) must pass with every row count
// matching the manifest.
//
// Needs PG_TEST_DSN (a superuser: it creates and drops one role and its databases) and
// PG_TEST_CONTAINER (the plan measures /dev/shm inside it). Skipped otherwise, and skipped
// when the container has enough shared memory that the failure cannot be reproduced.
func TestRestore_HNSWRebuildOnSmallShm_EndToEnd_Pg(t *testing.T) {
	dsn, container := os.Getenv("PG_TEST_DSN"), os.Getenv("PG_TEST_CONTAINER")
	if dsn == "" || container == "" {
		t.Skip("PG_TEST_DSN and PG_TEST_CONTAINER are needed for the shared-memory restore test")
	}
	ctx := context.Background()
	admin, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UnixNano() % 1_000_000_000
	role := fmt.Sprintf("cambrian_shm_e2e_%d", stamp)
	prefix := role
	adminExec := func(sql string) {
		t.Helper()
		c := admin.Copy()
		c.Database = "postgres"
		conn, err := pgx.ConnectConfig(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(ctx)
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	const rolePass = "shm-e2e-pass"
	adminExec("CREATE ROLE " + role + " LOGIN SUPERUSER PASSWORD '" + rolePass + "'")
	adminExec("ALTER ROLE " + role + " SET maintenance_work_mem = '128MB'")
	live, scratch, control := prefix+"_live", prefix+"_restore_drill", prefix+"_control"
	t.Cleanup(func() {
		for _, db := range []string{live, scratch, control} {
			adminExec("DROP DATABASE IF EXISTS " + db + " WITH (FORCE)")
		}
		adminExec("DROP ROLE IF EXISTS " + role)
	})
	adminExec("CREATE DATABASE " + live + " OWNER " + role)

	pg := PG{Host: admin.Host, Port: fmt.Sprint(admin.Port), User: role, Password: rolePass, DBName: live, Container: container}
	pc, err := pgxpool.ParseConfig(pg.connString(live))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := migrate.Migrate(ctx, pool, 1024); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Seed the real chunks table. The live index is built serially here, as a live store's
	// index never needs one big build; the restore is what builds it in one go. The text
	// column is wide enough that the heap passes min_parallel_table_scan_size, so the
	// server chooses a parallel build.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`DROP INDEX idx_chunks_embedding_cosine`,
		`INSERT INTO chunks (id, text, embedding)
		 SELECT 'c' || g, (SELECT string_agg(md5(g::text || '-' || i), '') FROM generate_series(1, 40) i),
		        (SELECT array_agg(random())::real[]::vector FROM generate_series(1, 1024) WHERE g > 0)
		 FROM generate_series(1, 8000) g`,
		`SET max_parallel_maintenance_workers = 0`,
		`CREATE INDEX idx_chunks_embedding_cosine ON chunks USING hnsw (embedding vector_cosine_ops) WITH (m = 24, ef_construction = 100)`,
	} {
		if _, err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	conn.Release()

	plan, err := pg.PlanIndexBuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Serial() {
		t.Skipf("the container has enough shared memory for a parallel build (%s); cannot reproduce ENG-2123", plan.Reason)
	}
	if !strings.Contains(plan.Reason, "/dev/shm") || !strings.Contains(plan.Reason, "shm_size") {
		t.Fatalf("plan reason does not tell the operator what to change: %q", plan.Reason)
	}

	root := t.TempDir()
	dir, m, err := Run(ctx, Options{Root: root, KeyRoot: filepath.Join(t.TempDir(), "keys"), Kind: KindManual, PG: pg})
	if err != nil {
		t.Fatalf("backup run: %v", err)
	}

	// Negative control: the restore as it was before ENG-2123 (server defaults).
	if err := pg.createDatabase(ctx, control, ""); err != nil {
		t.Fatal(err)
	}
	err = pg.WithDB(control).restoreArchive(ctx, filepath.Join(dir, "db.dump"), false)
	if err == nil || !strings.Contains(err.Error(), "shared memory") {
		t.Fatalf("restore without the index build plan should fail on a small /dev/shm, got %v", err)
	}

	// The drill plans the index build and passes.
	var notes []string
	res, err := Drill(ctx, dir, pg, scratch, RestoreOptions{Live: live,
		Logf: func(f string, a ...any) { notes = append(notes, fmt.Sprintf(f, a...)) }}, false)
	if err != nil {
		t.Fatalf("drill: %v", err)
	}
	if len(res.Mismatches) != 0 || !res.IndexBuild.Serial() {
		t.Fatalf("drill mismatches %v, plan %+v", res.Mismatches, res.IndexBuild)
	}
	var chunks int64
	for _, tc := range m.Tables {
		if tc.Table == "public.chunks" {
			chunks = tc.Rows
		}
	}
	if chunks != 8000 || res.Rows < 8000 {
		t.Fatalf("manifest chunks %d, drill rows %d", chunks, res.Rows)
	}
	if len(notes) == 0 || !strings.Contains(notes[0], "index build:") {
		t.Fatalf("drill did not log its index build plan: %v", notes)
	}
	t.Logf("drill: %d tables, %d rows; %s", res.Tables, res.Rows, notes[0])
}
