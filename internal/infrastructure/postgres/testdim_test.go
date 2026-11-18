package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/migrate"
)

// eventTestDim is the embedding dimension every Pg test in this package writes and
// searches at. It FOLLOWS the PG_TEST_DSN database: TestMain reads the dimension
// chunks.embedding was migrated at and uses that.
//
// It used to be the constant 1536 (CI's migrate dimension). On a test database
// migrated at the config's 1024 the tests then failed on every vector write, and
// newTestAdapter's ensureSchema at 1536 met a 1024 column: with no memory documents
// present ensureSchema RECREATES chunks at the new dimension (the documented
// non-destructive branch), so a test run silently reshaped the shared database and
// every later package met a 1536 table
// (T-20261113-core-postgres-pg-tests-fail-on-a-database). Tests must adapt to the
// database, never the database to the tests.
var eventTestDim = defaultTestEmbeddingDim

// defaultTestEmbeddingDim applies when there is no database to ask (no PG_TEST_DSN —
// every Pg test skips then) or the probe fails.
const defaultTestEmbeddingDim = 1536

// sharedTestDBDim is the dimension an EMPTY PG_TEST_DSN database is migrated at before
// the tests: the default config's embedder.dimensions (1024), the dimension the kernel
// and app Pg tests boot with and whose boot refuses a store at any other. Letting the
// first ensureSchema create an empty database at defaultTestEmbeddingDim instead left
// it at 1536 — or, after internal/migrate, half-migrated at 4 — for every package that
// ran after this one (T-20261118-core-pg-test-packages-fail-on-an-empty-pg-test).
// internal/migrate's TestMain restores an empty start to the same dimension.
var sharedTestDBDim = config.DefaultConfig().Embedder.Dimensions

// migrateEmptyTestDB migrates a database without a chunks table to HEAD at
// sharedTestDBDim through the production runner (migrate.Boot, under its advisory
// lock). Creating the schema in an empty database destroys nothing.
func migrateEmptyTestDB(ctx context.Context, dsn string) error {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	return migrate.Boot(ctx, pool, sharedTestDBDim, migrate.Policy{Mode: migrate.ModeAuto})
}

var vectorTypeDim = regexp.MustCompile(`^vector\((\d+)\)$`)

// parseVectorDim returns N for the pgvector type name "vector(N)", or 0 for anything
// else (an untyped "vector", a missing column).
func parseVectorDim(formatType string) int {
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

// probeChunksEmbeddingDim reads the migrated dimension of chunks.embedding, 0 when
// the table or column does not exist or the type carries no dimension.
func probeChunksEmbeddingDim(ctx context.Context, dsn string) (int, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return 0, err
	}
	defer pool.Close()
	var vecType *string
	if err := pool.QueryRow(ctx, `SELECT format_type(a.atttypid, a.atttypmod) FROM pg_attribute a
		WHERE a.attrelid = to_regclass('public.chunks') AND a.attname = 'embedding'`).Scan(&vecType); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil // no chunks table (to_regclass is NULL) — nothing to follow
		}
		return 0, err
	}
	if vecType == nil {
		return 0, nil
	}
	return parseVectorDim(*vecType), nil
}

func TestMain(m *testing.M) {
	if dsn := os.Getenv("PG_TEST_DSN"); dsn != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		dim, err := probeChunksEmbeddingDim(ctx, dsn)
		cancel()
		switch {
		case err != nil:
			// Not fatal: the Pg tests report their own connection errors; the
			// unit tests in this package need no database at all.
			fmt.Fprintf(os.Stderr, "internal/infrastructure/postgres: probe chunks.embedding dimension: %v (using %d)\n", err, defaultTestEmbeddingDim)
		case dim > 0:
			eventTestDim = dim
		default:
			// No chunks table: an empty database. Migrate it to HEAD at the shared
			// dimension, so the tests meet the full schema and leave the database
			// where the kernel and app packages can boot on it.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			err := migrateEmptyTestDB(ctx, dsn)
			cancel()
			if err != nil {
				fmt.Fprintf(os.Stderr, "internal/infrastructure/postgres: migrate the empty PG_TEST_DSN database at %d dims: %v\n", sharedTestDBDim, err)
				os.Exit(1)
			}
			eventTestDim = sharedTestDBDim
		}
	}
	os.Exit(m.Run())
}

// TestParseVectorDim pins the type-name parsing the dimension probe depends on.
func TestParseVectorDim(t *testing.T) {
	for in, want := range map[string]int{
		"vector(1024)": 1024,
		"vector(1536)": 1536,
		"vector(4)":    4,
		"vector":       0,
		"halfvec(768)": 0,
		"":             0,
	} {
		if got := parseVectorDim(in); got != want {
			t.Errorf("parseVectorDim(%q) = %d, want %d", in, got, want)
		}
	}
}
