package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// ENG-128 retention deletes against a real, migrated Postgres (PG_TEST_DSN).
// Rows use the "pgret-" prefix.
func retentionPool(t *testing.T) (*pgxpool.Pool, context.Context) {
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
	clean := func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM operator_audit WHERE id LIKE 'pgret-%'`)
		_, _ = pool.Exec(c, `DELETE FROM chunk_triplets WHERE chunk_id LIKE 'pgret-%'`)
		_, _ = pool.Exec(c, `DELETE FROM experiences WHERE id LIKE 'pgret-%'`)
	}
	clean()
	t.Cleanup(clean)
	return pool, ctx
}

func TestPg_PruneOperatorAuditBefore(t *testing.T) {
	pool, ctx := retentionPool(t)
	st, err := NewPgAuditStore(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i, age := range []time.Duration{400 * 24 * time.Hour, 380 * 24 * time.Hour, 24 * time.Hour} {
		id := "pgret-a" + string(rune('0'+i))
		if _, err := st.Record(ctx, domain.AuditEntry{ID: id, CommandID: id, At: now.Add(-age),
			Actor: "op", Role: "operator", ActionType: "x", TargetType: "t", TargetID: "1"}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := st.PruneOperatorAuditBefore(ctx, now.Add(-365*24*time.Hour), 1)
	if err != nil || n != 1 {
		t.Fatalf("first batch = %d, %v; want 1 (bounded)", n, err)
	}
	n, _ = st.PruneOperatorAuditBefore(ctx, now.Add(-365*24*time.Hour), 10)
	if n != 1 {
		t.Fatalf("second batch = %d, want the other old row", n)
	}
	var left int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM operator_audit WHERE id LIKE 'pgret-%'`).Scan(&left)
	if left != 1 {
		t.Errorf("rows left = %d, want the recent one", left)
	}
}

func TestPg_PruneChunkTripletsAndExperiencesBefore(t *testing.T) {
	pool, ctx := retentionPool(t)
	old := time.Now().UTC().Add(-90 * 24 * time.Hour)
	if _, err := pool.Exec(ctx, `INSERT INTO chunk_triplets (chunk_id,h,r,t,extracted_at) VALUES
		('pgret-c1','a','b','c',$1), ('pgret-c2','a','b','c',now())`, old); err != nil {
		t.Fatal(err)
	}
	if n, err := PruneChunkTripletsBefore(ctx, pool, time.Now().Add(-30*24*time.Hour), 100); err != nil || n != 1 {
		t.Fatalf("triplets pruned %d, %v; want 1", n, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO experiences (id, created_at) VALUES
		('pgret-e1', $1), ('pgret-e2', now())`, old.Format("2006-01-02 15:04:05")); err != nil {
		t.Fatal(err)
	}
	if n, err := PruneExperiencesBefore(ctx, pool, time.Now().Add(-30*24*time.Hour), 100); err != nil || n != 1 {
		t.Fatalf("experiences pruned %d, %v; want 1", n, err)
	}
}
