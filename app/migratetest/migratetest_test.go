package migratetest

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A malformed lane is refused before anything touches a database, exactly as
// Registry.AddMigrations refuses it.
func TestApplyLane_ValidatesFirst(t *testing.T) {
	bad := fstest.MapFS{"one.sql": {Data: []byte("SELECT 1;")}}
	if err := ApplyLane(context.Background(), nil, "probe", bad); err == nil {
		t.Fatal("a lane with a malformed file name must be refused")
	}
	good := fstest.MapFS{"0001_x.sql": {Data: []byte("SELECT 1;")}}
	if err := ApplyLane(context.Background(), nil, "core", good); err == nil {
		t.Fatal(`the reserved namespace "core" must be refused`)
	}
}

// Against a scratch database (created and dropped here): the lane applies into
// plugin_schema_migrations, a second apply is a no-op, and a database carrying
// a version the lane does not know is refused. PG_TEST_DSN names a server where
// the test may CREATE DATABASE.
func TestPg_ApplyLane(t *testing.T) {
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
	name := fmt.Sprintf("cambrian_test_migratetest_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Host, cfg.ConnConfig.Port = base.Host, base.Port
	cfg.ConnConfig.User, cfg.ConnConfig.Password = base.User, base.Password
	cfg.ConnConfig.TLSConfig, cfg.ConnConfig.Database = base.TLSConfig, name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
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

	lane := fstest.MapFS{
		"0001_baseline.sql": {Data: []byte("CREATE TABLE IF NOT EXISTS probe (id TEXT PRIMARY KEY);")},
		"0002_column.sql":   {Data: []byte("ALTER TABLE probe ADD COLUMN note TEXT;")},
	}
	if err := ApplyLane(ctx, pool, "probe", lane); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// 0002 is not idempotent SQL: a second apply that re-ran it would fail.
	if err := ApplyLane(ctx, pool, "probe", lane); err != nil {
		t.Fatalf("second apply must skip applied versions: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM plugin_schema_migrations WHERE namespace = 'probe'`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("ledger rows = %d (err %v), want 2", n, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO plugin_schema_migrations (namespace, version, name) VALUES ('probe', 7, 'newer')`); err != nil {
		t.Fatal(err)
	}
	if err := ApplyLane(ctx, pool, "probe", lane); err == nil || !strings.Contains(err.Error(), "ahead of this binary") {
		t.Fatalf("a database ahead of the lane must be refused, got %v", err)
	}
}
