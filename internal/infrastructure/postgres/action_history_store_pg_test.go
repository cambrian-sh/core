package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/testing/pgscratch"
)

// ENG-784 against a real, migrated Postgres (PG_TEST_DSN; 0066). Rows use the
// "pgah-" scope prefix and the "pgapt-" approval prefix.
func newActionHistoryStore(t *testing.T) (*PgActionHistoryStore, *pgxpool.Pool, context.Context) {
	t.Helper()
	dsn, _ := pgscratch.Migrated(t) // skips without PG_TEST_DSN; migrates a fresh database
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	st := NewPgActionHistoryStore(pool)
	if !st.Ready(ctx) {
		t.Skip("action_history table missing (run `orchestrator migrate up` first)")
	}
	clean := func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM action_history WHERE scope LIKE 'pgah-%' OR scope LIKE 'conversation:pgah-%'`)
		_, _ = pool.Exec(context.Background(), `DELETE FROM approvals WHERE tool_name LIKE 'pgapt-%'`)
	}
	clean()
	t.Cleanup(clean)
	return st, pool, ctx
}

// The adapter: an upsert by key, arguments back byte for byte, ids and approval
// kept, oldest first, window-bounded reads and a prune on write.
func TestPg_ActionHistoryStoreRoundTrip(t *testing.T) {
	st, pool, ctx := newActionHistoryStore(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	args := []byte(`{"to": "a@x.com",  "subject":"ü"}`)
	rec := domain.ActionRecord{Tool: "send_mail", Args: args, ApprovalID: "apr-1", IDs: []string{"message_id=m-1"}, At: now.Add(-time.Minute)}
	if err := st.RecordDone(ctx, "pgah-s1", rec, "pgah-k1", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	rec2 := domain.ActionRecord{Tool: "post", Args: []byte(`not json`), At: now}
	if err := st.RecordDone(ctx, "pgah-s1", rec2, "pgah-k2", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	// The same key again: one row, its time moved.
	rec.At = now.Add(-30 * time.Second)
	if err := st.RecordDone(ctx, "pgah-s1", rec, "pgah-k1", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := st.LoadDone(ctx, "pgah-s1", now.Add(-time.Hour), 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Tool != "send_mail" || string(got[0].Args) != string(args) ||
		got[0].ApprovalID != "apr-1" || strings.Join(got[0].IDs, ",") != "message_id=m-1" ||
		got[0].State != domain.ActionDone || !got[0].At.Equal(rec.At) || got[1].Tool != "post" || len(got[1].IDs) != 0 {
		t.Fatalf("loaded = %+v", got)
	}
	if got, _ := st.LoadDone(ctx, "pgah-s1", now.Add(-time.Hour), 1); len(got) != 1 || got[0].Tool != "post" {
		t.Fatalf("limit must keep the most recent: %+v", got)
	}
	if got, _ := st.LoadDone(ctx, "pgah-s1", now.Add(-10*time.Second), 200); len(got) != 1 {
		t.Fatalf("since must bound the read: %+v", got)
	}
	// A write prunes every row older than keepAfter, of any scope.
	old := domain.ActionRecord{Tool: "x", Args: []byte(`{}`), At: now.Add(-3 * time.Hour)}
	if err := st.RecordDone(ctx, "pgah-s2", old, "pgah-k3", now.Add(-4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordDone(ctx, "pgah-s3", domain.ActionRecord{Tool: "y", Args: []byte(`{}`), At: now}, "pgah-k4", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM action_history WHERE key = 'pgah-k3'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("expired row kept: %d %v", n, err)
	}
}

// The 0066 approvals.duplicate_notice column round-trips through put, get, list
// and resolve.
func TestPg_ApprovalDuplicateNoticeRoundTrip(t *testing.T) {
	_, pool, ctx := newActionHistoryStore(t)
	st := NewPgApprovalStore(pool)
	now := time.Now().UTC().Truncate(time.Millisecond)
	it := domain.ApprovalItem{ID: "apr-pgapt-notice", NaturalKey: "pgapt-notice", ToolName: "pgapt-create",
		State: domain.ApprovalPending, RaisedAt: now, ExpiresAt: now.Add(time.Hour),
		DuplicateNotice: "Possible duplicate of pgapt-create (approval apr-0) — done at …; identical arguments."}
	if err := st.PutApproval(ctx, it); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetApproval(ctx, it.ID)
	if err != nil || got.DuplicateNotice != it.DuplicateNotice {
		t.Fatalf("get = %q %v", got.DuplicateNotice, err)
	}
	it.State, it.ResolvedAt, it.ResolvedBy = domain.ApprovalApproved, now, "op"
	if ok, err := st.ResolveApproval(ctx, it); !ok || err != nil {
		t.Fatalf("resolve: %v %v", ok, err)
	}
	list, err := st.ListApprovals(ctx, domain.ApprovalQuery{States: []domain.ApprovalState{domain.ApprovalApproved}})
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range list {
		if l.ID == it.ID && l.DuplicateNotice == it.DuplicateNotice {
			return
		}
	}
	t.Fatalf("resolved item listed without its notice: %+v", list)
}

type pgahHandler struct{ result []byte }

func (h pgahHandler) Execute(context.Context, domain.ToolCall) ([]byte, error) { return h.result, nil }

// End to end through the real path, across a restart: kernel run 1 (executor,
// hub, Postgres approval store, Postgres action history) has a dangerous write
// approved; kernel run 2 — new executor, new hub, new history over the same
// database — raises an approval for the same write whose notice names run 1's
// call, and a third hub (another restart) lists that pending item with its notice.
func TestPg_DuplicateNoticeSurvivesAKernelRestart(t *testing.T) {
	hist, pool, ctx := newActionHistoryStore(t)
	approvals := NewPgApprovalStore(pool)
	ctx = domain.WithConversationID(ctx, "pgah-conv")

	kernelRun := func() (*domain.AddressedHub, *domain.ToolExecutor) {
		reg := domain.NewInMemoryToolRegistry()
		reg.Register(domain.SystemTool{Name: "pgapt-create_issue", Dangerous: true})
		grants := domain.NewInMemoryGrantsStore()
		grants.Set("a", []domain.ToolGrant{{Tool: "pgapt-create_issue"}})
		hub := domain.NewAddressedHub(domain.AddressedHubConfig{Store: approvals})
		te, err := domain.NewToolExecutor(domain.ToolExecutorControls{Registry: reg, Grants: grants,
			Authz: domain.AllowAllAuthorizer{}, Approval: hub}, domain.ToolExecutor{
			Handler: pgahHandler{result: []byte(`{"key":"OPS-11"}`)},
			History: domain.NewDurableActionHistory(24*time.Hour, nil, hist),
		})
		if err != nil {
			t.Fatal(err)
		}
		return hub, te
	}
	call := func(te *domain.ToolExecutor, summary string) <-chan domain.ToolCallResponse {
		out := make(chan domain.ToolCallResponse, 1)
		go func() {
			out <- te.Execute(ctx, domain.ToolCallRequest{AgentID: "a", ToolName: "pgapt-create_issue",
				ArgsJSON: []byte(`{"project_key":"OPS","summary":"` + summary + `"}`)})
		}()
		return out
	}
	raised := func(evs <-chan domain.ApprovalEvent) domain.ApprovalItem {
		for {
			select {
			case ev := <-evs:
				if ev.Type == domain.ApprovalEventRaised {
					return ev.Item
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no approval was raised")
			}
		}
	}
	watch := domain.ApprovalWatch{For: domain.OperatorApprover("op"), Resolutions: true}

	hub1, te1 := kernelRun()
	evs1, stop1 := hub1.Watch(watch)
	r1 := call(te1, "INC-1040 (Ashvale) - Database outage")
	it1 := raised(evs1)
	if it1.DuplicateNotice != "" {
		t.Fatalf("run 1's approval has a notice: %q", it1.DuplicateNotice)
	}
	if _, err := hub1.Answer(ctx, it1.ID, domain.ApprovalAnswer{Approved: true, By: domain.OperatorApprover("op")}); err != nil {
		t.Fatal(err)
	}
	if r := <-r1; r.Denied || r.Error != "" {
		t.Fatalf("run 1: %+v", r)
	}
	stop1()

	hub2, te2 := kernelRun() // the restart
	evs2, stop2 := hub2.Watch(watch)
	defer stop2()
	r2 := call(te2, "INC-1040 - Database outage")
	it2 := raised(evs2)
	if !strings.HasPrefix(it2.DuplicateNotice, domain.DuplicateNoticePrefix) ||
		!strings.Contains(it2.DuplicateNotice, "approval "+it1.ID) || !strings.Contains(it2.DuplicateNotice, "key=OPS-11") {
		t.Fatalf("run 2's notice = %q", it2.DuplicateNotice)
	}

	hub3 := domain.NewAddressedHub(domain.AddressedHubConfig{Store: approvals}) // another restart
	listed, err := hub3.List(ctx, domain.ApprovalQuery{States: []domain.ApprovalState{domain.ApprovalPending}})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, l := range listed {
		found = found || (l.ID == it2.ID && l.DuplicateNotice == it2.DuplicateNotice)
	}
	if !found {
		t.Fatalf("pending item listed after a restart without its notice: %+v", listed)
	}
	if _, err := hub2.Answer(ctx, it2.ID, domain.ApprovalAnswer{Approved: true, By: domain.OperatorApprover("op")}); err != nil {
		t.Fatal(err)
	}
	if r := <-r2; r.Denied || r.Error != "" {
		t.Fatalf("the notice never refuses: %+v", r)
	}
}
