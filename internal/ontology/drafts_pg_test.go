package ontology

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
)

// Integration tests for agent drafts over real Postgres (PG_TEST_DSN;
// migrations 0040 + 0041) with the real approval hub on the Postgres approval
// store and the shipped approval policy (ontology_change → "*" → operator).

type pgDraftRig struct {
	pool  *pgxpool.Pool
	o     *Ontology
	hub   *domain.AddressedHub
	d     *Drafts
	audit *auditLog
}

func newPgDraftRig(t *testing.T) (pgDraftRig, context.Context) {
	t.Helper()
	pool, ctx := pgPool(t)
	clean := func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `TRUNCATE ontology_draft_events, ontology_drafts`)
		_, _ = pool.Exec(bg, `DELETE FROM approvals WHERE kind = $1`, domain.ApprovalKindOntologyChange)
	}
	clean()
	t.Cleanup(clean)
	o, err := Load(ctx, pool, seedDoc(), authorities())
	if err != nil {
		t.Fatal(err)
	}
	hub := domain.NewAddressedHub(domain.AddressedHubConfig{Store: postgres.NewPgApprovalStore(pool)})
	audit := &auditLog{}
	d, err := NewDrafts(pool, o, hub, audit.record, nil)
	if err != nil {
		t.Fatal(err)
	}
	return pgDraftRig{pool: pool, o: o, hub: hub, d: d, audit: audit}, ctx
}

func (r pgDraftRig) answer(t *testing.T, ctx context.Context, id string, approved bool, by string) domain.ApprovalItem {
	t.Helper()
	it, err := r.hub.Answer(ctx, id, domain.ApprovalAnswer{Approved: approved, By: domain.OperatorApprover(by)})
	if err != nil {
		t.Fatalf("answer %s: %v", id, err)
	}
	stored, err := r.hub.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	_ = it
	return stored
}

func (r pgDraftRig) declRows(t *testing.T, ctx context.Context, ns string) []string {
	t.Helper()
	rows, err := r.pool.Query(ctx, `SELECT version, author FROM ontology_declarations WHERE namespace = $1 ORDER BY version`, ns)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v int64
		var a string
		if err := rows.Scan(&v, &a); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("v%d:%s", v, a))
	}
	return out
}

func (r pgDraftRig) state(t *testing.T, ctx context.Context, id string) domain.OntologyDraft {
	t.Helper()
	d, ok, err := pgDraftStore{pool: r.pool}.getDraft(ctx, id)
	if err != nil || !ok {
		t.Fatalf("draft %s: %v %v", id, ok, err)
	}
	return d
}

func TestPg_Drafts_AppendOnlyIsEnforced(t *testing.T) {
	r, ctx := newPgDraftRig(t)
	res, err := r.d.Propose(ctx, agentP, aliceP, DraftProposal{Namespace: "drafts-ao", Change: invoiceChange()})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`UPDATE ontology_drafts SET reason = 'x' WHERE draft_id = $1`,
		`DELETE FROM ontology_drafts WHERE draft_id = $1`,
		`UPDATE ontology_draft_events SET actor = 'x' WHERE draft_id = $1`,
		`DELETE FROM ontology_draft_events WHERE draft_id = $1`,
	} {
		if _, err := r.pool.Exec(ctx, q, res.Draft.ID); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("%s: want the append-only refusal, got %v", q, err)
		}
	}
	store := pgDraftStore{pool: r.pool}
	// A concurrent retry's insert of the same draft is a no-op, not an error.
	if ins, err := store.insertDraft(ctx, res.Draft); err != nil || ins {
		t.Fatalf("re-insert: inserted=%v err=%v", ins, err)
	}
	// A second terminal state is not recorded: the first one stands.
	for i, st := range []domain.OntologyDraftState{domain.OntologyDraftRejected, domain.OntologyDraftApplied} {
		ins, err := store.appendEvent(ctx, draftEvent{DraftID: res.Draft.ID, State: st, Actor: "x", At: time.Now()})
		if err != nil || ins != (i == 0) {
			t.Fatalf("terminal %s: inserted=%v err=%v", st, ins, err)
		}
	}
	if s := r.state(t, ctx, res.Draft.ID); s.State != domain.OntologyDraftRejected {
		t.Fatalf("first terminal state must win: %s", s.State)
	}
}

func TestPg_Drafts_ApproveAppliesAsTheApproverAndTheNextWriteUsesIt(t *testing.T) {
	r, ctx := newPgDraftRig(t)
	const ns = "drafts-approve"
	cleanNamespace(t, r.pool, ns)
	store := postgres.NewPgKnowledgeStore(r.pool, r.o)
	discount := orderItem(ns, "order/1", "open", nil, domain.StatementValue{Predicate: "discount", Type: "number", Number: 12})
	if _, _, err := store.PutItem(ctx, discount); !errors.Is(err, domain.ErrKindRefused) {
		t.Fatalf("an undeclared predicate must be refused before the draft applies: %v", err)
	}
	addDiscount := domain.OntologyDoc{Kinds: []domain.KindSpec{{Kind: "order", Predicates: map[string]domain.ValueSpec{
		"status": {Type: "text", Enum: []string{"open", "shipped", "lost"}}, "total": {Type: "number"}, "discount": {Type: "number"}}}}}
	res, err := r.d.Propose(ctx, agentP, aliceP, DraftProposal{Namespace: ns, Change: addDiscount, Reason: "billing"})
	if err != nil {
		t.Fatal(err)
	}
	// A retried draft is the same row and the same approval.
	again, err := r.d.Propose(ctx, agentP, aliceP, DraftProposal{Namespace: ns, Change: addDiscount, Reason: "billing"})
	if err != nil || again.Created || again.Draft.ID != res.Draft.ID || again.Draft.ApprovalID != res.Draft.ApprovalID {
		t.Fatalf("retry: %+v %v", again, err)
	}
	var drafts, events int
	_ = r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM ontology_drafts), (SELECT count(*) FROM ontology_draft_events)`).Scan(&drafts, &events)
	if drafts != 1 || events != 1 {
		t.Fatalf("a retry wrote rows: %d drafts, %d events", drafts, events)
	}
	if got := r.declRows(t, ctx, ns); len(got) != 0 {
		t.Fatalf("drafting wrote a declaration: %v", got)
	}
	// The approval is routed by the shipped policy: an operator, not the beneficiary.
	ap, err := r.hub.Get(ctx, res.Draft.ApprovalID)
	if err != nil || ap.State != domain.ApprovalPending || ap.RuleSummary() != "operator" || ap.Object != res.Draft.ID {
		t.Fatalf("raised approval: %+v %v", ap, err)
	}
	if _, err := r.hub.Answer(ctx, ap.ID, domain.ApprovalAnswer{Approved: true,
		By: domain.Approver{Principal: aliceP}}); !errors.Is(err, domain.ErrApprovalNotAddressee) {
		t.Fatalf("a non-operator must not answer: %v", err)
	}
	before := r.o.Current(ns).Version
	if it := r.answer(t, ctx, ap.ID, true, "bob"); it.State != domain.ApprovalApproved || strings.Contains(it.Detail, "failed") {
		t.Fatalf("approval: %+v", it)
	}
	after := r.o.Current(ns).Version
	if after <= before {
		t.Fatalf("version did not increment: %d -> %d", before, after)
	}
	if got := r.declRows(t, ctx, ns); len(got) != 1 || got[0] != fmt.Sprintf("v%d:user:bob", after) {
		t.Fatalf("declaration rows: %v (the asserter must be the approver, never the agent)", got)
	}
	s := r.state(t, ctx, res.Draft.ID)
	if s.State != domain.OntologyDraftApplied || s.StateBy != "user:bob" || s.AppliedVersion != after || s.ApprovalID != ap.ID {
		t.Fatalf("draft: %+v", s)
	}
	// The very next write validates against the approved declarations.
	if _, _, err := store.PutItem(ctx, discount); err != nil {
		t.Fatalf("write after approval: %v", err)
	}
	// A reboot replays the approved change.
	o2, err := Load(ctx, r.pool, seedDoc(), authorities())
	if err != nil || o2.Current(ns).Version != after {
		t.Fatalf("reload: %v", err)
	}
	if got := r.audit.actions(); fmt.Sprint(got) != "[ontology_draft_pending@agent:planner ontology_draft_applied@user:bob]" {
		t.Fatalf("audit: %v", got)
	}
}

func TestPg_Drafts_DenyAndExpiryWriteNoDeclaration(t *testing.T) {
	r, ctx := newPgDraftRig(t)
	const ns = "drafts-deny"
	res, err := r.d.Propose(ctx, agentP, aliceP, DraftProposal{Namespace: ns, Change: invoiceChange()})
	if err != nil {
		t.Fatal(err)
	}
	r.answer(t, ctx, res.Draft.ApprovalID, false, "bob")
	// Expiry: a draft raised by a hub whose clock has run past the window.
	past := time.Now().Add(-8 * 24 * time.Hour)
	oldHub := domain.NewAddressedHub(domain.AddressedHubConfig{Store: postgres.NewPgApprovalStore(r.pool), Now: func() time.Time { return past }})
	oldDrafts, err := NewDrafts(r.pool, r.o, oldHub, r.audit.record, nil)
	if err != nil {
		t.Fatal(err)
	}
	exp, err := oldDrafts.Propose(ctx, agentP, aliceP, DraftProposal{Namespace: ns, Change: invoiceChange(), ClientRef: "late"})
	if err != nil {
		t.Fatal(err)
	}
	r.hub.Start(ctx) // the hub's sweep expires it
	r.hub.Stop()
	if err := r.d.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got := r.declRows(t, ctx, ns); len(got) != 0 {
		t.Fatalf("deny/expiry wrote a declaration: %v", got)
	}
	if s := r.state(t, ctx, res.Draft.ID); s.State != domain.OntologyDraftRejected || s.StateBy != "bob" {
		t.Fatalf("denied draft: %+v", s)
	}
	if s := r.state(t, ctx, exp.Draft.ID); s.State != domain.OntologyDraftExpired {
		t.Fatalf("expired draft: %+v", s)
	}
}

func TestPg_Drafts_StaleBaseIsSupersededNotApplied(t *testing.T) {
	r, ctx := newPgDraftRig(t)
	const ns = "drafts-stale"
	tighten := domain.OntologyDoc{Kinds: []domain.KindSpec{{Kind: "order", Predicates: map[string]domain.ValueSpec{
		"status": {Type: "text", Enum: []string{"open", "shipped"}}, "total": {Type: "number"}}}}}
	res, err := r.d.Propose(ctx, agentP, aliceP, DraftProposal{Namespace: ns, Change: tighten})
	if err != nil {
		t.Fatal(err)
	}
	// An operator narrows the enum further first: the draft would now WIDEN it.
	if _, err := r.o.Apply(ctx, ns, domain.OntologyDoc{Kinds: []domain.KindSpec{{Kind: "order", Predicates: map[string]domain.ValueSpec{
		"status": {Type: "text", Enum: []string{"open"}}, "total": {Type: "number"}}}}}, "user:op", false); err != nil {
		t.Fatal(err)
	}
	rowsBefore := r.declRows(t, ctx, ns)
	it := r.answer(t, ctx, res.Draft.ApprovalID, true, "bob")
	if !strings.Contains(it.Detail, "approved, but applying it failed") || !strings.Contains(it.Detail, "moved from version") {
		t.Fatalf("the approval must carry the reason: %q", it.Detail)
	}
	if got := r.declRows(t, ctx, ns); fmt.Sprint(got) != fmt.Sprint(rowsBefore) {
		t.Fatalf("a superseded draft wrote a declaration: %v -> %v", rowsBefore, got)
	}
	s := r.state(t, ctx, res.Draft.ID)
	if s.State != domain.OntologyDraftSuperseded || !strings.Contains(s.Detail, "additive") {
		t.Fatalf("draft: %+v", s)
	}
}

func TestPg_Drafts_ConcurrentApprovalsAreDeterministic(t *testing.T) {
	r, ctx := newPgDraftRig(t)
	const ns = "drafts-race"
	// Two drafts over the same predicate, each a tightening of the base; once one
	// is applied the other no longer classifies as drafted.
	enum := func(vals ...string) domain.OntologyDoc {
		return domain.OntologyDoc{Kinds: []domain.KindSpec{{Kind: "order", Predicates: map[string]domain.ValueSpec{
			"status": {Type: "text", Enum: vals}, "total": {Type: "number"}}}}}
	}
	a, err := r.d.Propose(ctx, agentP, aliceP, DraftProposal{Namespace: ns, Change: enum("open", "shipped")})
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.d.Propose(ctx, agentP, aliceP, DraftProposal{Namespace: ns, Change: enum("open", "lost")})
	if err != nil {
		t.Fatal(err)
	}
	// Two independent drafts (different kinds) both apply.
	c, err := r.d.Propose(ctx, agentP, aliceP, DraftProposal{Namespace: ns, Change: invoiceChange()})
	if err != nil {
		t.Fatal(err)
	}
	d, err := r.d.Propose(ctx, agentP, aliceP, DraftProposal{Namespace: ns, Change: domain.OntologyDoc{
		Relations: []domain.RelationSpec{{Name: "ships_to", Family: domain.LinkFamilyRelation}}}})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i, dr := range []DraftResult{a, b, c, d} {
		wg.Add(1)
		go func(id, by string) {
			defer wg.Done()
			if _, err := r.hub.Answer(ctx, id, domain.ApprovalAnswer{Approved: true, By: domain.OperatorApprover(by)}); err != nil {
				t.Errorf("answer: %v", err)
			}
		}(dr.Draft.ApprovalID, fmt.Sprintf("op%d", i))
	}
	wg.Wait()
	sa, sb := r.state(t, ctx, a.Draft.ID), r.state(t, ctx, b.Draft.ID)
	applied := 0
	for _, s := range []domain.OntologyDraft{sa, sb} {
		switch s.State {
		case domain.OntologyDraftApplied:
			applied++
		case domain.OntologyDraftSuperseded:
		default:
			t.Fatalf("conflicting draft in state %s", s.State)
		}
	}
	if applied != 1 {
		t.Fatalf("exactly one of two conflicting drafts applies, got %d (%s, %s)", applied, sa.State, sb.State)
	}
	if sc, sd := r.state(t, ctx, c.Draft.ID), r.state(t, ctx, d.Draft.ID); sc.State != domain.OntologyDraftApplied ||
		sd.State != domain.OntologyDraftApplied || sc.AppliedVersion == sd.AppliedVersion {
		t.Fatalf("independent drafts: %+v / %+v", sc, sd)
	}
	if got := r.declRows(t, ctx, ns); len(got) != 3 {
		t.Fatalf("three declarations written (one of the conflicting pair + two independent), got %v", got)
	}
}
