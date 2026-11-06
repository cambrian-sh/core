package postgres

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/skill/lifecycle"
)

// ADR-0139 D13 against a real, migrated Postgres (PG_TEST_DSN; schema from
// `orchestrator migrate up`, 0029_skill_lifecycle). Every row uses the "pgskt-"
// skill-name prefix and the "pgskt-" run/command prefix, removed before and after.
// The service in internal/skill/lifecycle runs the same rules on its in-memory
// test store; this holds the durable adapter to them.

func newSkillLifecycleStore(t *testing.T) (*PgSkillLifecycleStore, *pgxpool.Pool, context.Context) {
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
	st := NewPgSkillLifecycleStore(pool)
	if !st.Ready(ctx) {
		t.Skip("skill lifecycle tables missing (run `orchestrator migrate up` first)")
	}
	clean := func() {
		c := context.Background()
		tx, err := pool.Begin(c)
		if err != nil {
			return
		}
		defer func() { _ = tx.Rollback(c) }()
		// skill_ledger is append-only by trigger; test cleanup alone disables
		// triggers, for this transaction only.
		_, _ = tx.Exec(c, `SET LOCAL session_replication_role = replica`)
		for _, q := range []string{
			`DELETE FROM skill_ledger WHERE skill_name LIKE 'pgskt-%'`,
			`DELETE FROM skill_run_uses WHERE skill_name LIKE 'pgskt-%'`,
			`DELETE FROM skill_versions WHERE skill_name LIKE 'pgskt-%'`,
			`DELETE FROM skill_records WHERE name LIKE 'pgskt-%'`,
			`DELETE FROM skill_proposals WHERE name LIKE 'pgskt-%'`,
		} {
			_, _ = tx.Exec(c, q)
		}
		_ = tx.Commit(c)
	}
	clean()
	t.Cleanup(clean)
	return st, pool, ctx
}

func pgSkillMD(name, body string) string {
	return "---\nname: " + name + "\ndescription: A test skill.\ntrust: official\n---\n" + body
}

func TestPg_SkillLifecycle_ProposeApproveRollbackUsage(t *testing.T) {
	st, pool, ctx := newSkillLifecycleStore(t)
	reg := domain.NewInMemorySkillRegistry()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	svc, err := lifecycle.New(st, reg, nil, lifecycle.Config{ProposalsEnabled: true}, nil, clock, nil)
	if err != nil {
		t.Fatal(err)
	}
	propose := func(body, base string) lifecycle.ProposeResult {
		t.Helper()
		r, err := svc.Propose(ctx, lifecycle.ProposeInput{Name: "pgskt-tidy", SkillMD: pgSkillMD("pgskt-tidy", body),
			BaseVersion: base, AgentID: "agent-a", RunID: "pgskt-run-0"})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	p1 := propose("1. v1 body.", "")
	if again := propose("1. v1 body.", ""); again.ProposalID != p1.ProposalID || !again.Accepted {
		t.Fatalf("deterministic id: %+v vs %+v", again, p1)
	}
	if _, ok := reg.Get("pgskt-tidy"); ok {
		t.Fatal("pending served")
	}
	a1, err := svc.Decide(ctx, lifecycle.DecideInput{CommandID: "pgskt-c1", ProposalID: p1.ProposalID, Approve: true, Actor: "op", Reason: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if sk, ok := reg.Get("pgskt-tidy"); !ok || sk.Trust != domain.SkillSourceAgentProposed || sk.Version != "1" {
		t.Fatalf("approved skill = %+v ok=%v", sk, ok)
	}
	dup, err := svc.Decide(ctx, lifecycle.DecideInput{CommandID: "pgskt-c1", ProposalID: p1.ProposalID, Approve: true, Actor: "op", Reason: "ok"})
	if err != nil || !dup.Deduped || dup.LedgerEntryID != a1.LedgerEntryID {
		t.Fatalf("dedupe = %+v err=%v", dup, err)
	}

	e1, e2 := propose("1. edit A.", "1"), propose("1. edit B.", "1")
	if _, err := svc.Decide(ctx, lifecycle.DecideInput{CommandID: "pgskt-c2", ProposalID: e1.ProposalID, Approve: true, Actor: "op", Reason: "ok"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Decide(ctx, lifecycle.DecideInput{CommandID: "pgskt-c3", ProposalID: e2.ProposalID, Approve: true, Actor: "op", Reason: "ok"}); !errors.Is(err, lifecycle.ErrFailedPrecondition) {
		t.Fatalf("stale base: %v", err)
	}

	if _, err := svc.SetLock(ctx, "pgskt-c4", "pgskt-tidy", true, "op", "stable"); err != nil {
		t.Fatal(err)
	}
	if r := propose("1. v3.", "2"); r.Rejection != lifecycle.RejectLocked {
		t.Fatalf("locked = %+v", r)
	}
	rb, err := svc.Rollback(ctx, "pgskt-c5", "pgskt-tidy", a1.LedgerEntryID, "op", "revert")
	if err != nil || rb.Version != "1" {
		t.Fatalf("rollback = %+v err=%v", rb, err)
	}
	entries, total, err := svc.Ledger(ctx, "pgskt-tidy", 0, 0)
	if err != nil || total != 4 || entries[0].Mutation != domain.SkillMutationRolledBack || entries[0].RollbackOf != a1.LedgerEntryID {
		t.Fatalf("ledger = %+v total=%d err=%v", entries, total, err)
	}

	// The database itself refuses to rewrite history.
	if _, err := pool.Exec(ctx, `UPDATE skill_ledger SET reason = 'x' WHERE id = $1`, a1.LedgerEntryID); err == nil {
		t.Fatal("skill_ledger accepted an UPDATE")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM skill_ledger WHERE id = $1`, a1.LedgerEntryID); err == nil {
		t.Fatal("skill_ledger accepted a DELETE")
	}

	// A stale ExpectSeq is a conflict, not a duplicate.
	if _, err := st.Commit(ctx, domain.SkillCommit{ExpectSeq: 1, Entry: domain.SkillLedgerEntry{SkillName: "pgskt-tidy", Mutation: "locked", Actor: "x", At: now}}); !errors.Is(err, domain.ErrSkillLedgerConflict) {
		t.Fatalf("stale commit: %v", err)
	}

	// Usage: two loads in one run, one success, settled once.
	for i := 0; i < 2; i++ {
		if err := svc.RecordUse(ctx, "pgskt-tidy", "pgskt-run-1"); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := svc.RecordRunOutcome(ctx, "pgskt-run-1", true); err != nil {
			t.Fatal(err)
		}
	}
	rec, _, _ := st.GetSkill(ctx, "pgskt-tidy")
	if rec.UseCount != 2 || rec.SuccessCount != 1 || rec.LastUsedAt.IsZero() || !rec.Locked {
		t.Fatalf("record = %+v", rec)
	}

	// A reboot materialises the rolled-back content.
	reg2 := domain.NewInMemorySkillRegistry()
	svc2, _ := lifecycle.New(st, reg2, nil, lifecycle.Config{ProposalsEnabled: true}, nil, clock, nil)
	if err := svc2.Materialize(ctx); err != nil {
		t.Fatal(err)
	}
	if sk, ok := reg2.Get("pgskt-tidy"); !ok || sk.Version != "1" {
		t.Fatalf("materialised = %+v ok=%v", sk, ok)
	}
}
