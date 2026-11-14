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
// every Pg test skips then) or the database carries no chunks table yet (a fresh
// database the first ensureSchema will create at this dimension).
const defaultTestEmbeddingDim = 1536

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
		if err != nil {
			// Not fatal: the Pg tests report their own connection errors; the
			// unit tests in this package need no database at all.
			fmt.Fprintf(os.Stderr, "internal/infrastructure/postgres: probe chunks.embedding dimension: %v (using %d)\n", err, defaultTestEmbeddingDim)
		} else if dim > 0 {
			eventTestDim = dim
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
