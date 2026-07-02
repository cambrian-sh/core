package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Integration test for the Postgres operator_audit store (ADR-0047 0047-24).
// Skips without PG_TEST_DSN; runs in CI. Covers the UNIQUE command_id dedup and
// filtered query.
func TestPgAuditStore_RecordDedupAndQuery(t *testing.T) {
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping integration test that requires PostgreSQL")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS operator_audit")

	store, err := NewPgAuditStore(ctx, pool)
	if err != nil {
		t.Fatalf("NewPgAuditStore: %v", err)
	}

	e := domain.AuditEntry{
		ID: "id-1", CommandID: "cmd-1", At: time.Now().UTC(), Actor: "alice", Role: "operator",
		ActionType: "set_tool_grant", TargetType: "agent", TargetID: "agent-1",
		Before: "[]", After: `["web_search"]`, Reason: "research", Result: "ok",
	}

	deduped, err := store.Record(ctx, e)
	if err != nil || deduped {
		t.Fatalf("first record: deduped=%v err=%v", deduped, err)
	}
	// Same command_id (different id) → deduped, no second row.
	e2 := e
	e2.ID = "id-2"
	deduped, err = store.Record(ctx, e2)
	if err != nil || !deduped {
		t.Fatalf("retry should dedup: deduped=%v err=%v", deduped, err)
	}

	rows, err := store.Query(ctx, domain.AuditFilter{Actor: "alice"})
	if err != nil || len(rows) != 1 || rows[0].CommandID != "cmd-1" {
		t.Fatalf("query: rows=%+v err=%v", rows, err)
	}
	// A non-matching filter returns nothing.
	if rows, _ := store.Query(ctx, domain.AuditFilter{Actor: "bob"}); len(rows) != 0 {
		t.Fatalf("expected no rows for bob, got %d", len(rows))
	}
}
