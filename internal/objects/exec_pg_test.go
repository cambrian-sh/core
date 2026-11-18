package objects

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
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

// The row-tags probe (ADR-0158 stage 4) reads the request's own snapshot: no
// row-classified fence class / link → the plan stays the P4 one; one → the
// row-tags fence. Runs in a transaction that is rolled back, so the shared
// dictionary is left as it was found.
func TestPg_RowTaggedProbeFollowsTheSnapshot(t *testing.T) {
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
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // test-only: leave the shared dictionary untouched
	for _, q := range []string{
		`DELETE FROM object_fence_classes WHERE cardinality(row_tags) > 0`,
		`DELETE FROM links WHERE cardinality(classification) > 0 OR cardinality(supporting_evidence) > 0`,
	} {
		if _, err := tx.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	scope := &domain.TagPredicate{RequiredTags: []string{"a"}}
	withLink := checked{Links: []linkFilter{{}}}
	probe := func(step string, c checked, want rowTagged) {
		t.Helper()
		_, got, err := visibleClasses(ctx, tx, c, scope)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s: rowTagged %+v, want %+v", step, got, want)
		}
		if _, got, _ := visibleClasses(ctx, tx, c, domain.ScopeMaintenance); got != (rowTagged{}) {
			t.Fatalf("%s: maintenance probed %+v (it renders no fence)", step, got)
		}
	}
	probe("nothing row-classified", withLink, rowTagged{})
	if _, err := tx.Exec(ctx, `INSERT INTO object_fence_classes (tags, parties, row_tags) VALUES ('{probe:x}', '{}', '{cls:restricted}')`); err != nil {
		t.Fatal(err)
	}
	probe("a row-classified fence class", withLink, rowTagged{Values: true})
	// ENG-2049: a link with supporting evidence needs the full link fence too.
	if _, err := tx.Exec(ctx, `INSERT INTO links (id, family, from_ref, to_ref, relation, state, mechanism, asserted_by, asserted_at, supporting_evidence)
		VALUES ('probe-link-supported', 'relation', 'entity:a/1', 'entity:b/2', 'probe_rel', 'confirmed', 'record', 't', now(), '{probe-ev}')`); err != nil {
		t.Fatal(err)
	}
	probe("a link with supporting evidence", withLink, rowTagged{Values: true, Links: true})
	if _, err := tx.Exec(ctx, `DELETE FROM links WHERE id = 'probe-link-supported'`); err != nil {
		t.Fatal(err)
	}
	probe("supported link gone", withLink, rowTagged{Values: true})
	if _, err := tx.Exec(ctx, `INSERT INTO links (id, family, from_ref, to_ref, relation, state, mechanism, asserted_by, asserted_at, classification)
		VALUES ('probe-link', 'relation', 'entity:a/1', 'entity:b/1', 'probe_rel', 'confirmed', 'record', 't', now(), '{cls:restricted}')`); err != nil {
		t.Fatal(err)
	}
	probe("a row-classified link", withLink, rowTagged{Values: true, Links: true})
	probe("a request without link filters does not ask about links", checked{}, rowTagged{Values: true})
}
