package postgres

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/migrate"
)

// ENG-2061 through the production boot path: NewPgVectorAdapter on a database that
// has NO pgvector extension (a fresh Postgres server's database). Before the fix the
// pool's AfterConnect failed with "vector type not found" and the baseline that
// creates the extension never ran.

// bareScratchDB creates an empty database WITHOUT the vector extension on the
// PG_TEST_DSN server (dropped at the end) and returns a config pointing at it.
func bareScratchDB(t *testing.T) (*config.Config, func(q string) bool) {
	t.Helper()
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping the Postgres-backed boot test")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect PG_TEST_DSN: %v", err)
	}
	defer admin.Close(ctx)
	name := fmt.Sprintf("cambrian_test_noext_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create scratch database: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			t.Logf("drop %s: %v", name, err)
			return
		}
		defer c.Close(context.Background())
		if _, err := c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Logf("drop %s: %v", name, err)
		}
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("PG_TEST_DSN: %v", err)
	}
	u.Path = "/" + name
	cfg := config.DefaultConfig()
	cfg.Database.Host = u.Hostname()
	cfg.Database.Port = u.Port()
	cfg.Database.User = u.User.Username()
	cfg.Database.Password, _ = u.User.Password()
	cfg.Database.DBName = name
	cfg.Embedder.Dimensions = 4
	exists := func(q string) bool {
		c, err := pgx.Connect(ctx, u.String())
		if err != nil {
			t.Fatalf("connect scratch database: %v", err)
		}
		defer c.Close(ctx)
		var ok bool
		if err := c.QueryRow(ctx, q).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	return cfg, exists
}

const (
	qHasVector = `SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector')`
	qHasChunks = `SELECT to_regclass('public.chunks') IS NOT NULL`
)

func TestPg_NewPgVectorAdapter_FreshDatabaseWithoutPgvectorBoots(t *testing.T) {
	cfg, exists := bareScratchDB(t)
	if exists(qHasVector) {
		t.Fatal("the scratch database must start without pgvector")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	a, err := NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		t.Fatalf("a default boot on a fresh database must create pgvector and migrate: %v", err)
	}
	a.Close()
	if !exists(qHasVector) || !exists(qHasChunks) {
		t.Fatal("the boot must leave the extension and the migrated schema behind")
	}
}

func TestPg_NewPgVectorAdapter_NoAutoMigrateRefusesNamingTheSQL(t *testing.T) {
	cfg, exists := bareScratchDB(t)
	cfg.Storage.AutoMigrate = false
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := NewPgVectorAdapter(ctx, cfg)
	if !errors.Is(err, migrate.ErrVectorExtensionMissing) {
		t.Fatalf("want ErrVectorExtensionMissing, got %v", err)
	}
	if !strings.Contains(err.Error(), migrate.VectorExtensionSQL) {
		t.Fatalf("the refusal must name the SQL to run: %v", err)
	}
	if exists(qHasVector) {
		t.Fatal("a boot with the runner off must not create the extension")
	}
}
