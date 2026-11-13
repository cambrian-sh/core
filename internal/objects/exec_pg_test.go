package objects

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The read transaction's settings are part of the existence rule's contract
// (T-20261113-unfiltered-listobjects-group-list-under-a): one snapshot for the
// visible classes and the query (REPEATABLE READ), no JIT, and custom plans so
// the bound class array stays a hashed constant. The statement_timeout
// bounds it (ADR-0149 D6).
func TestPg_ReadTransactionSettings(t *testing.T) {
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" || !strings.Contains(strings.ToLower(dsn), "test") {
		t.Skip("PG_TEST_DSN not set to a disposable (…test…) database")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	r := &Reader{pool: pool}
	var iso, jit, cache, timeout, readOnly string
	if err := r.read(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT current_setting('transaction_isolation'), current_setting('jit'),
			current_setting('plan_cache_mode'), current_setting('statement_timeout'), current_setting('transaction_read_only')`).
			Scan(&iso, &jit, &cache, &timeout, &readOnly)
	}); err != nil {
		t.Fatal(err)
	}
	if iso != "repeatable read" || jit != "off" || cache != "force_custom_plan" || timeout != "5s" || readOnly != "on" {
		t.Fatalf("read tx: isolation %q jit %q plan_cache_mode %q statement_timeout %q read_only %q", iso, jit, cache, timeout, readOnly)
	}
	// SET LOCAL: nothing leaks into the pooled session afterwards.
	var after string
	if err := pool.QueryRow(ctx, `SELECT current_setting('plan_cache_mode')`).Scan(&after); err != nil || after == "force_custom_plan" {
		t.Fatalf("settings leaked past the transaction: %q %v", after, err)
	}
}
