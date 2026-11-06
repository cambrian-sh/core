package postgres

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// Per-agent skill grants (decisions/skill-access-grants.md) against a real,
// migrated Postgres (PG_TEST_DSN; 0031_skill_grants). Rows use the "pgsgt-" agent
// prefix, removed before and after.
func newSkillGrantStore(t *testing.T) (*PgSkillGrantStore, *pgxpool.Pool, context.Context) {
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
	st := NewPgSkillGrantStore(pool)
	if !st.Ready(ctx) {
		t.Skip("skill grant tables missing (run `orchestrator migrate up` first)")
	}
	clean := func() {
		c := context.Background()
		tx, err := pool.Begin(c)
		if err != nil {
			return
		}
		defer func() { _ = tx.Rollback(c) }()
		// skill_grant_events is append-only by trigger; test cleanup alone disables
		// triggers, for this transaction only.
		_, _ = tx.Exec(c, `SET LOCAL session_replication_role = replica`)
		for _, q := range []string{
			`DELETE FROM skill_grant_events WHERE agent_id LIKE 'pgsgt-%'`,
			`DELETE FROM skill_access_requests WHERE agent_id LIKE 'pgsgt-%'`,
			`DELETE FROM skill_grants WHERE agent_id LIKE 'pgsgt-%'`,
		} {
			_, _ = tx.Exec(c, q)
		}
		_ = tx.Commit(c)
	}
	clean()
	t.Cleanup(clean)
	return st, pool, ctx
}

func TestPg_SkillGrants_GrantRevokeRequestAudit(t *testing.T) {
	st, pool, ctx := newSkillGrantStore(t)
	at := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	agent := "pgsgt-analyst"

	if err := st.OpenRequest(ctx, domain.SkillAccessRequest{AgentID: agent, SkillName: "proposed-x", RunID: "run-1", Rationale: "need", RequestedAt: at}); err != nil {
		t.Fatal(err)
	}
	pend, err := st.ListRequests(ctx, domain.SkillGrantFilter{AgentID: agent, State: domain.SkillAccessPending})
	if err != nil || len(pend) != 1 || pend[0].ID != domain.SkillAccessRequestID(agent, "proposed-x") {
		t.Fatalf("pending = %+v err=%v", pend, err)
	}

	g := domain.SkillGrant{AgentID: agent, SkillName: "proposed-x", Source: domain.SkillGrantSourceOperator, GrantedBy: "op", GrantedAt: at, Reason: "ok"}
	if changed, err := st.Grant(ctx, g, "pgsgt-cmd-1"); err != nil || !changed {
		t.Fatalf("grant: %v %v", changed, err)
	}
	if changed, err := st.Grant(ctx, g, ""); err != nil || changed {
		t.Fatalf("re-grant must be a no-op: %v %v", changed, err)
	}
	active, _ := st.ActiveGrants(ctx, agent)
	if len(active) != 1 || active[0].ID != domain.SkillGrantID(agent, "proposed-x") || !active[0].Active() {
		t.Fatalf("active = %+v", active)
	}
	reqs, _ := st.ListRequests(ctx, domain.SkillGrantFilter{AgentID: agent})
	if len(reqs) != 1 || reqs[0].State != domain.SkillAccessGranted {
		t.Fatalf("the grant must resolve the request: %+v", reqs)
	}
	if e, ok, err := st.EventByCommand(ctx, "pgsgt-cmd-1"); err != nil || !ok || e.Action != domain.SkillGrantActionGranted {
		t.Fatalf("event by command: %+v %v %v", e, ok, err)
	}

	if err := st.Revoke(ctx, agent, "proposed-x", "op", "done", "pgsgt-cmd-2", at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := st.Revoke(ctx, agent, "proposed-x", "op", "again", "", at.Add(2*time.Hour)); !errors.Is(err, domain.ErrSkillGrantNotFound) {
		t.Fatalf("revoking a revoked grant = %v", err)
	}
	if active, _ := st.ActiveGrants(ctx, agent); len(active) != 0 {
		t.Fatalf("revoked grant still active: %+v", active)
	}
	all, _ := st.ListGrants(ctx, domain.SkillGrantFilter{AgentID: agent, IncludeRevoked: true})
	if len(all) != 1 || all[0].Active() || all[0].RevokedBy != "op" {
		t.Fatalf("all = %+v", all)
	}
	// Re-grant re-activates the same row.
	if changed, err := st.Grant(ctx, g, ""); err != nil || !changed {
		t.Fatalf("re-grant after revoke: %v %v", changed, err)
	}
	if err := st.DenyRequest(ctx, agent, "other", "op", at); err != nil {
		t.Fatal(err)
	}

	evs, _ := st.ListEvents(ctx, domain.SkillGrantFilter{AgentID: agent}, 0)
	if len(evs) != 5 {
		t.Fatalf("audit rows = %d (%+v), want 5 (requested, granted, revoked, granted, denied)", len(evs), evs)
	}
	// Append-only in the database.
	if _, err := pool.Exec(ctx, `UPDATE skill_grant_events SET reason = 'x' WHERE agent_id = $1`, agent); err == nil {
		t.Fatal("skill_grant_events must refuse UPDATE")
	}
	// A duplicate operator command_id cannot append a second row.
	if _, err := st.Grant(ctx, domain.SkillGrant{AgentID: agent, SkillName: "y", Source: "operator", GrantedAt: at}, "pgsgt-cmd-1"); err == nil {
		t.Fatal("a reused command_id must not append")
	}
}
