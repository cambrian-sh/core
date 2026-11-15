package postgres

import (
	"context"
	"os"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// The durable operator tool grants (ENG-1116) against a real, migrated Postgres
// (PG_TEST_DSN; 0052_tool_grants). Rows use the "pgtg-" agent prefix.
func newToolGrantStore(t *testing.T) (*PgToolGrantStore, *pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping integration test that requires PostgreSQL")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	st := NewPgToolGrantStore(pool)
	if !st.Ready(ctx) {
		t.Skip("tool_grants missing (run `orchestrator migrate up` first)")
	}
	clean := func() { _, _ = pool.Exec(context.Background(), `DELETE FROM tool_grants WHERE agent_id LIKE 'pgtg-%'`) }
	clean()
	t.Cleanup(clean)
	return st, pool, ctx
}

func toolNames(gs []domain.ToolGrant) []string {
	out := make([]string, 0, len(gs))
	for _, g := range gs {
		out = append(out, g.Tool)
	}
	return out
}

func TestPg_ToolGrantStore_ReplaceReadRevokeAndPolicyRoundTrip(t *testing.T) {
	st, pool, ctx := newToolGrantStore(t)
	pol := domain.ToolResourcePolicy{Network: domain.NetworkPolicy{AllowDomains: []string{"example.com"}}}
	if err := st.ReplaceGrants(ctx, "pgtg-a", []domain.ToolGrant{{Tool: "propose_knowledge"}, {Tool: "fetch", Policy: pol}, {Tool: "fetch"}}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GrantsFor(ctx, "pgtg-a")
	if err != nil || !slices.Equal(toolNames(got), []string{"fetch", "propose_knowledge"}) {
		t.Fatalf("GrantsFor = %v, %v", toolNames(got), err)
	}
	if !slices.Equal(got[0].Policy.Network.AllowDomains, []string{"example.com"}) {
		t.Fatalf("policy did not round-trip (first entry per tool wins): %+v", got[0].Policy)
	}

	// A fresh store over a new pool (a restart) reads the same rows.
	pool2, err := pgxpool.New(ctx, os.Getenv("PG_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool2.Close()
	again, err := NewPgToolGrantStore(pool2).GrantsFor(ctx, "pgtg-a")
	if err != nil || !slices.Equal(toolNames(again), []string{"fetch", "propose_knowledge"}) {
		t.Fatalf("after reopen = %v, %v", toolNames(again), err)
	}

	// Idempotent replace keeps granted_at; revoke deletes the row.
	var before string
	if err := pool.QueryRow(ctx, `SELECT granted_at::text FROM tool_grants WHERE agent_id='pgtg-a' AND tool='propose_knowledge'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceGrants(ctx, "pgtg-a", []domain.ToolGrant{{Tool: "propose_knowledge"}}); err != nil {
		t.Fatal(err)
	}
	var after string
	if err := pool.QueryRow(ctx, `SELECT granted_at::text FROM tool_grants WHERE agent_id='pgtg-a' AND tool='propose_knowledge'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("granted_at moved on an idempotent replace: %s → %s", before, after)
	}
	if got, _ := st.GrantsFor(ctx, "pgtg-a"); !slices.Equal(toolNames(got), []string{"propose_knowledge"}) {
		t.Fatalf("after revoking fetch = %v", toolNames(got))
	}
	if err := st.ReplaceGrants(ctx, "pgtg-a", nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GrantsFor(ctx, "pgtg-a"); len(got) != 0 {
		t.Fatalf("revoke-all left %v", toolNames(got))
	}

	if err := st.ReplaceGrants(ctx, "pgtg-b", []domain.ToolGrant{{Tool: "propose_ontology_change"}}); err != nil {
		t.Fatal(err)
	}
	all, err := st.AllGrants(ctx)
	if err != nil || len(all["pgtg-b"]) != 1 || len(all["pgtg-a"]) != 0 {
		t.Fatalf("AllGrants = %v, %v", all, err)
	}
	if none, err := st.GrantsFor(ctx, ""); err != nil || none != nil {
		t.Fatalf("empty agent = %v, %v", none, err)
	}
	if err := st.ReplaceGrants(ctx, "", nil); err == nil {
		t.Fatal("empty agent id accepted")
	}
	if err := st.ReplaceGrants(ctx, "pgtg-a", []domain.ToolGrant{{Tool: ""}}); err == nil {
		t.Fatal("empty tool accepted")
	}
}

// A store over a closed pool returns an error, never an empty set.
func TestPg_ToolGrantStore_ClosedPoolErrors(t *testing.T) {
	_, _, ctx := newToolGrantStore(t)
	pool, err := pgxpool.New(ctx, os.Getenv("PG_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	st := NewPgToolGrantStore(pool)
	pool.Close()
	if got, err := st.GrantsFor(ctx, "pgtg-a"); err == nil {
		t.Fatalf("closed pool: GrantsFor = %v with no error", got)
	}
	if err := st.ReplaceGrants(ctx, "pgtg-a", nil); err == nil {
		t.Fatal("closed pool: ReplaceGrants returned no error")
	}
}
