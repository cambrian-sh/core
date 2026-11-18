package postgres

import (
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/testing/pgscratch"
)

// Integration test for the Postgres operator_audit store (ADR-0047 0047-24).
// Skips without PG_TEST_DSN; runs in CI. Covers the UNIQUE command_id dedup and
// filtered query.
func TestPgAuditStore_RecordDedupAndQuery(t *testing.T) {
	// Its own schema: NewPgAuditStore creates operator_audit there instead of the test
	// dropping the migrated table every later test needs
	// (T-20261114-postgres-adapter-tests-drop-migrated-tables).
	pool, ctx := pgscratch.Pool(t, "operator_audit_test", pgscratch.Options{})

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
	// ENG-666: Actors matches any recorded form of one person and replaces Actor.
	legacy := e
	legacy.ID, legacy.CommandID, legacy.Actor = "id-3", "cmd-3", "agent:alice"
	if _, err := store.Record(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	rows, err = store.Query(ctx, domain.AuditFilter{Actor: "ignored", Actors: []string{"alice", "user:alice", "agent:alice"}})
	if err != nil || len(rows) != 2 {
		t.Fatalf("Actors must match both forms: rows=%+v err=%v", rows, err)
	}
}
