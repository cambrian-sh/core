package postgres

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/cambrian-sh/core/internal/testing/pgscratch"
)

// eventTestDim is the embedding dimension every Pg test in this package writes and
// searches at. It FOLLOWS the PG_TEST_DSN database: TestMain migrates the database to
// HEAD (pgscratch.EnsureMigrated) and uses the dimension chunks.embedding carries —
// on an empty database, the shared 1024 (pgscratch.SharedTestDBDim).
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
// every Pg test skips then).
const defaultTestEmbeddingDim = 1536

func TestMain(m *testing.M) {
	if dsn := os.Getenv("PG_TEST_DSN"); dsn != "" {
		// Every run migrates the database to HEAD itself (ENG-2100): a database behind
		// HEAD used to turn the "<table> missing (run `orchestrator migrate up` first)"
		// tests into silent skips, and a non-disposable one skipped ~20 more.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		dim, err := pgscratch.EnsureMigrated(ctx, dsn)
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "internal/infrastructure/postgres: %v\n", err)
			os.Exit(1)
		}
		eventTestDim = dim
	}
	os.Exit(m.Run())
}
