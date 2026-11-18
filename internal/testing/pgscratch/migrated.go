package pgscratch

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/migrate"
)

// Every core Postgres test runs on a PG_TEST_DSN database that the test suite migrates
// itself, never on one that "someone ran `orchestrator migrate up` on first"
// (T-20261118-internal-evidence-pg-outbox-tests-fail-on-an). Before this helper, a
// package's Pg tests passed or failed depending on which package had happened to
// migrate the shared database earlier in the run (internal/evidence FAILED on an empty
// database, identity/memorynotes/app SKIPPED with "run migrate up"), and about twenty
// tests skipped silently when the database name lacked "test".
//
// Migrated brings the database to HEAD through the production runner (migrate.Migrate,
// under its advisory lock) once per test binary, at the dimension the database already
// carries — or, on an empty database, at the default config's embedder.dimensions
// (1024), the dimension every kernel and app boot test starts from. Tests then follow
// the returned dimension, never the reverse (tests adapt to the database,
// T-20261113-core-postgres-pg-tests-fail-on-a-database).

// disposableDB is the guard every helper applies before writing: a test database's name
// says so. It is a hard failure, not a skip — a skip here was the trap that let a
// misnamed database turn ~20 Pg tests silently green.
var disposableDB = regexp.MustCompile(`(?i)test`)

// DatabaseName returns the database named in dsn (URL or keyword/value form).
func DatabaseName(dsn string) (string, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return "", fmt.Errorf("parse PG_TEST_DSN: %w", err)
	}
	return cfg.Database, nil
}

// CheckDisposable refuses a DSN whose database name does not contain "test".
func CheckDisposable(dsn string) error {
	name, err := DatabaseName(dsn)
	if err != nil {
		return err
	}
	if !disposableDB.MatchString(name) {
		return fmt.Errorf("refusing to touch database %q: PG_TEST_DSN must name a disposable database whose name contains \"test\" (e.g. cambrian_test)", name)
	}
	return nil
}

var (
	migratedMu  sync.Mutex
	migratedDim = map[string]int{} // dsn → dimension, once migrated in this process
)

// EnsureMigrated refuses a non-disposable database, then migrates it to HEAD (once per
// process per DSN) and returns the embedding dimension it is migrated at. For TestMain;
// tests call Migrated.
func EnsureMigrated(ctx context.Context, dsn string) (int, error) {
	migratedMu.Lock()
	defer migratedMu.Unlock()
	if dim, ok := migratedDim[dsn]; ok {
		return dim, nil
	}
	if err := CheckDisposable(dsn); err != nil {
		return 0, err
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return 0, fmt.Errorf("connect PG_TEST_DSN: %w", err)
	}
	defer pool.Close()
	dim, err := probeChunksEmbeddingDim(ctx, pool)
	if err != nil {
		return 0, fmt.Errorf("probe chunks.embedding dimension: %w", err)
	}
	if dim == 0 {
		dim = SharedTestDBDim
	}
	if err := migrate.Migrate(ctx, pool, dim); err != nil {
		return 0, fmt.Errorf("migrate the PG_TEST_DSN database to HEAD at %d dims: %w", dim, err)
	}
	migratedDim[dsn] = dim
	return dim, nil
}

// SharedTestDBDim is the dimension an EMPTY PG_TEST_DSN database is migrated at: the
// default config's embedder.dimensions (1024). CI migrates its database at the same
// value, so the kernel/app boot tests (which refuse a store at any other dimension
// than their config's) and the adapter tests meet one schema.
var SharedTestDBDim = config.DefaultConfig().Embedder.Dimensions

// Migrated skips the test when PG_TEST_DSN is unset (the message keeps "PG_TEST_DSN not
// set": CI's Postgres gate fails the job on it), fails it when the database is not
// disposable or cannot be migrated, and returns the DSN and the embedding dimension the
// database is migrated at.
func Migrated(t testing.TB) (dsn string, dim int) {
	t.Helper()
	dsn = os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping integration test that requires PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	dim, err := EnsureMigrated(ctx, dsn)
	if err != nil {
		t.Fatalf("pgscratch: %v", err)
	}
	return dsn, dim
}

// ApplyToConfig points a config's database block at dsn and its embedder at dim, so a
// kernel/app boot test opens the migrated test database at the dimension it carries.
func ApplyToConfig(t testing.TB, cfg *config.Config, dsn string, dim int) {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("PG_TEST_DSN: %v", err)
	}
	cfg.Database.Host = u.Hostname()
	cfg.Database.Port = u.Port()
	cfg.Database.User = u.User.Username()
	cfg.Database.Password, _ = u.User.Password()
	cfg.Database.DBName = strings.TrimPrefix(u.Path, "/")
	cfg.Embedder.Dimensions = dim
}

var vectorTypeDim = regexp.MustCompile(`^vector\((\d+)\)$`)

// ParseVectorDim returns N for the pgvector type name "vector(N)", or 0 for anything
// else (an untyped "vector", a missing column).
func ParseVectorDim(formatType string) int {
	m := vectorTypeDim.FindStringSubmatch(formatType)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}

// probeChunksEmbeddingDim reads the migrated dimension of public.chunks.embedding, 0
// when the table or column does not exist or the type carries no dimension.
func probeChunksEmbeddingDim(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var vecType *string
	if err := pool.QueryRow(ctx, `SELECT format_type(a.atttypid, a.atttypmod) FROM pg_attribute a
		WHERE a.attrelid = to_regclass('public.chunks') AND a.attname = 'embedding'`).Scan(&vecType); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil // no chunks table (to_regclass is NULL) — an empty database
		}
		return 0, err
	}
	if vecType == nil {
		return 0, nil
	}
	return ParseVectorDim(*vecType), nil
}
