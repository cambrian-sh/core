package memorynotes

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// Integration tests against a real, migrated Postgres (PG_TEST_DSN; migration 0030).
// Rows use the "nttest-" owner prefix and are removed before and after.

func newPgTestStore(t *testing.T) (*PgStore, context.Context) {
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
	st, err := NewPgStore(ctx, pool)
	if err != nil {
		t.Skipf("memory_notes schema missing (run `orchestrator migrate up` first): %v", err)
	}
	clean := func() {
		for _, q := range []string{
			`DELETE FROM memory_notes WHERE owner_id LIKE 'nttest-%'`,
			`DELETE FROM memory_notes_history WHERE owner_id LIKE 'nttest-%'`,
			`DELETE FROM memory_notes_budgets WHERE owner_id LIKE 'nttest-%'`,
		} {
			if _, err := pool.Exec(context.Background(), q); err != nil {
				t.Fatalf("clean: %v", err)
			}
		}
	}
	clean()
	t.Cleanup(clean)
	return st, ctx
}

func TestPg_MemoryNotes_CASAndHistory(t *testing.T) {
	st, ctx := newPgTestStore(t)
	key := Key{Scope: ScopePersonal, OwnerID: "nttest-alice"}
	if d, err := st.Get(ctx, key); err != nil || d.Revision != 0 || d.Exists() {
		t.Fatalf("absent: %+v %v", d, err)
	}
	d1, err := st.Put(ctx, key, "one", 0, WriteMeta{Actor: "a", Reason: "r1", Op: "append"})
	if err != nil || d1.Revision != 1 {
		t.Fatalf("insert: %+v %v", d1, err)
	}
	if _, err := st.Put(ctx, key, "dup", 0, WriteMeta{Op: "append"}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("second insert at rev 0: want conflict, got %v", err)
	}
	if _, err := st.Put(ctx, key, "stale", 5, WriteMeta{Op: "append"}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale update: want conflict, got %v", err)
	}
	if _, err := st.Put(ctx, key, "two", 1, WriteMeta{Actor: "b", Op: "replace_section"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Delete(ctx, key, 2, WriteMeta{Actor: "operator:ops", Reason: "cleanup", Op: "delete"}); err != nil {
		t.Fatal(err)
	}
	d, err := st.Get(ctx, key)
	if err != nil || d.Exists() || !d.Deleted || d.Revision != 3 {
		t.Fatalf("after delete: %+v %v", d, err)
	}
	if l, _ := st.List(ctx, ListFilter{OwnerID: key.OwnerID}); len(l) != 0 {
		t.Fatalf("deleted doc listed: %+v", l)
	}
	if d4, err := st.Put(ctx, key, "again", 3, WriteMeta{Op: "set"}); err != nil || d4.Revision != 4 {
		t.Fatalf("re-create continues the revision: %+v %v", d4, err)
	}
	h, err := st.History(ctx, key)
	if err != nil || len(h) != 4 {
		t.Fatalf("history: %+v %v", h, err)
	}
	if h[2].Op != "delete" || h[2].Content != "two" || h[2].Reason != "cleanup" {
		t.Fatalf("delete keeps the cleared text: %+v", h[2])
	}
}

func TestPg_MemoryNotes_BudgetsAndService(t *testing.T) {
	st, ctx := newPgTestStore(t)
	svc, err := New(st, ScannerFunc(func(s string) (bool, string) {
		return strings.Contains(s, "IGNORE PREVIOUS"), "marker"
	}), domain.AllowAllAuthorizer{}, Config{PersonalBudgetChars: 50, OrgBudgetChars: 50})
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{ID: "nttest-bob", AgentID: "planner"}
	r, err := svc.Propose(ctx, p, Proposal{Op: OpAppend, Section: "Prefs", Content: "terse"})
	if err != nil || !r.Accepted {
		t.Fatalf("propose: %+v %v", r, err)
	}
	s1, _ := svc.Snapshot(ctx, "nttest-run", p)
	if r, _ := svc.Propose(ctx, p, Proposal{Op: OpAppend, Section: "Prefs", Content: "metric"}); !r.Accepted {
		t.Fatalf("second: %+v", r)
	}
	if s2, _ := svc.Snapshot(ctx, "nttest-run", p); s2.Version != s1.Version {
		t.Fatal("snapshot not frozen over Postgres")
	}
	if err := svc.SetBudget(ctx, Caller{ID: "ops"}, ScopePersonal, "nttest-bob", 5, false); err != nil {
		t.Fatal(err)
	}
	full, err := svc.Get(ctx, Caller{ID: "ops"}, Key{Scope: ScopePersonal, OwnerID: "nttest-bob"})
	if err != nil || !full.BudgetOverridden || full.Budget != 5 || !full.OverBudget || full.UpdatedBy != "agent:planner for nttest-bob" {
		t.Fatalf("budget override: %+v %v", full, err)
	}
	if r, _ := svc.Propose(ctx, p, Proposal{Op: OpAppend, Content: "x"}); r.Rejection != RejectOverBudget {
		t.Fatalf("over lowered budget: %+v", r)
	}
	s3, _ := svc.Snapshot(ctx, "nttest-run-2", p)
	if len(s3.Documents) != 1 || !s3.Documents[0].Truncated || len([]rune(s3.Documents[0].Content)) != 5 {
		t.Fatalf("truncated snapshot: %+v", s3)
	}
}
