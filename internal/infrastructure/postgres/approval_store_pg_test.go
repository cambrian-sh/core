package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// The durable approval hub store (ADR-0142 D11) against a real, migrated
// Postgres (PG_TEST_DSN; 0033_approvals). Rows use the "pgapt-" key prefix.
func newApprovalStore(t *testing.T) (*PgApprovalStore, context.Context) {
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
	st := NewPgApprovalStore(pool)
	if !st.Ready(ctx) {
		t.Skip("approval tables missing (run `orchestrator migrate up` first)")
	}
	clean := func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM approvals WHERE natural_key LIKE 'pgapt-%' OR tool_name LIKE 'pgapt-%'`)
		_, _ = pool.Exec(context.Background(), `DELETE FROM standing_approvals WHERE resource LIKE 'pgapt-%'`)
	}
	clean()
	t.Cleanup(clean)
	return st, ctx
}

// A pending approval survives a "restart" (a second hub over the same tables):
// it is answerable there by an addressee only, and its standing grant persists.
func TestPg_ApprovalHubDurableAcrossRestart(t *testing.T) {
	st, ctx := newApprovalStore(t)
	h1 := domain.NewAddressedHub(domain.AddressedHubConfig{Store: st})
	it, err := h1.Raise(ctx, domain.ApprovalRequest{
		Kind: domain.ApprovalKindMachineConsent, ToolName: "pgapt-write", Machine: "laptop",
		Beneficiary: domain.UserPrincipal("alice"), Candidates: []string{"laptop"},
	})
	if err != nil {
		t.Fatal(err)
	}
	h2 := domain.NewAddressedHub(domain.AddressedHubConfig{Store: st}) // the restart
	got, err := h2.Get(ctx, it.ID)
	if err != nil || got.State != domain.ApprovalPending || got.RuleSummary() != "self" ||
		got.Beneficiary != domain.UserPrincipal("alice") || len(got.Candidates) != 1 {
		t.Fatalf("after restart: %+v %v", got, err)
	}
	if _, err := h2.Answer(ctx, it.ID, domain.ApprovalAnswer{Approved: true, By: domain.OperatorApprover("root")}); err == nil {
		t.Fatal("an operator answered alice's self-ruled approval")
	}
	res, err := h2.Answer(ctx, it.ID, domain.ApprovalAnswer{Approved: true, Standing: true, StandingTTL: time.Hour,
		By: domain.Approver{Principal: domain.UserPrincipal("alice")}})
	if err != nil || res.State != domain.ApprovalApproved {
		t.Fatalf("alice's answer: %+v %v", res, err)
	}
	if _, err := h2.Answer(ctx, it.ID, domain.ApprovalAnswer{Approved: false, By: domain.Approver{Principal: domain.UserPrincipal("alice")}}); err == nil {
		t.Fatal("a resolved approval was answered twice")
	}
	h3 := domain.NewAddressedHub(domain.AddressedHubConfig{Store: st})
	list, err := h3.ListStanding(ctx, domain.StandingQuery{Scope: "person:alice"})
	if err != nil || len(list) != 1 || list[0].GrantedBy.Principal != domain.UserPrincipal("alice") || list[0].ExpiresAt.IsZero() {
		t.Fatalf("standing after restart: %+v %v", list, err)
	}
	d, err := h3.Request(ctx, domain.ApprovalRequest{
		Kind: domain.ApprovalKindMachineConsent, ToolName: "pgapt-write", Machine: "laptop",
		Beneficiary: domain.UserPrincipal("alice"), ArgsPreview: "other",
	})
	if err != nil || !d.Approved {
		t.Fatalf("the persisted standing approval must pre-approve: %+v %v", d, err)
	}
	if _, err := h3.RevokeStanding(ctx, list[0].ID, domain.Approver{Principal: domain.UserPrincipal("alice")}, "done"); err != nil {
		t.Fatal(err)
	}
	if l, _ := h3.ListStanding(ctx, domain.StandingQuery{Scope: "person:alice"}); len(l) != 0 {
		t.Fatalf("revoked standing still listed active: %+v", l)
	}
	pending, err := st.ListApprovals(ctx, domain.ApprovalQuery{States: []domain.ApprovalState{domain.ApprovalPending}})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pending {
		if p.ID == it.ID {
			t.Fatal("a resolved item is listed pending")
		}
	}
}
