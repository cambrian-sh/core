package authz

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0145 D1: every ledger read, by id or by query, goes through EnforcingLedger,
// applies the READER's predicate, fails closed without one, and reports a
// forbidden entry as absent (AGENTS.md §2.4/§2.5).

type memLedger struct{ rows []domain.LedgerEntry }

func (m *memLedger) Get(_ context.Context, id string) (*domain.LedgerEntry, error) {
	for i := range m.rows {
		if m.rows[i].ID == id {
			e := m.rows[i]
			return &e, nil
		}
	}
	return nil, nil
}

func (m *memLedger) Find(_ context.Context, q domain.LedgerQuery) ([]domain.LedgerEntry, error) {
	var out []domain.LedgerEntry
	for _, e := range m.rows {
		if q.RunID != "" && e.RunID != q.RunID {
			continue
		}
		out = append(out, e)
		if q.Limit > 0 && len(out) == q.Limit {
			break
		}
	}
	return out, nil
}

func ledgerFixture() *EnforcingLedger {
	return NewEnforcingLedger(&memLedger{rows: []domain.LedgerEntry{
		{ID: "led_alice", RunID: "r", Tags: []string{domain.OwnerTag("alice")}, Body: []byte("alice's balance 1250.50")},
		{ID: "led_bob", RunID: "r", Tags: []string{domain.OwnerTag("bob")}, Body: []byte("bob's balance 80")},
		{ID: "led_hr", RunID: "r", Tags: []string{"hr"}, Body: []byte("salary table")},
		{ID: "led_open", RunID: "r", Body: []byte("public")},
	}}, nil)
}

// readerFor resolves the reader's predicate the way the kernel does (ReadFilter
// under a ctx carrying the conversation owner the agent works for).
func readerFor(ctx context.Context, a domain.Authorizer, agent string) context.Context {
	eff, _ := a.ReadFilter(ctx, domain.AgentPrincipal(agent), domain.SurfaceRef{Kind: domain.SurfaceAgent})
	return domain.WithScope(ctx, eff)
}

func TestEnforcingLedger_NoPredicateFailsClosed(t *testing.T) {
	l := ledgerFixture()
	if _, err := l.Get(context.Background(), "led_open"); !errors.Is(err, ErrScopeMissing) {
		t.Fatalf("Get without a predicate: err = %v, want ErrScopeMissing", err)
	}
	if _, err := l.Find(context.Background(), domain.LedgerQuery{RunID: "r"}); !errors.Is(err, ErrScopeMissing) {
		t.Fatalf("Find without a predicate: err = %v, want ErrScopeMissing", err)
	}
}

// Cross-principal: an agent working Bob's conversation cannot read Alice's entry,
// by id or by query; it reads as absent, not as a denial.
func TestEnforcingLedger_CrossPrincipalReadsAsAbsent(t *testing.T) {
	l := ledgerFixture()
	bobCtx := readerFor(domain.WithConversationOwner(context.Background(), "bob"), domain.AllowAllAuthorizer{}, "worker")
	if e, err := l.Get(bobCtx, "led_alice"); err != nil || e != nil {
		t.Fatalf("bob read alice's entry: %+v %v", e, err)
	}
	if e, err := l.Get(bobCtx, "led_bob"); err != nil || e == nil {
		t.Fatalf("bob cannot read his own entry: %v", err)
	}
	rows, err := l.Find(bobCtx, domain.LedgerQuery{RunID: "r"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == "led_alice" {
			t.Fatal("Find surfaced alice's entry to bob")
		}
	}
	if len(rows) != 3 {
		t.Fatalf("bob sees %d rows, want 3 (his, hr, open under the OSS bypass)", len(rows))
	}
}

// Act-for (ADR-0142): an operator acting for Alice works Alice's conversation, so
// the kernel resolves the conversation owner to Alice and her entries are readable;
// the operator acting for nobody reads none of the owner-fenced ones.
func TestEnforcingLedger_ActForReadsTheOwnersEntries(t *testing.T) {
	l := ledgerFixture()
	actFor := domain.WithActForActor(domain.WithConversationOwner(context.Background(), "alice"), "operator-1")
	ctx := readerFor(actFor, domain.AllowAllAuthorizer{}, "chat")
	if e, _ := l.Get(ctx, "led_alice"); e == nil {
		t.Fatal("acting for alice, her entry must be readable")
	}
	if e, _ := l.Get(ctx, "led_bob"); e != nil {
		t.Fatal("acting for alice, bob's entry must read as absent")
	}
	noOwner := readerFor(context.Background(), domain.AllowAllAuthorizer{}, "chat")
	if e, _ := l.Get(noOwner, "led_alice"); e != nil {
		t.Fatal("a reader acting for nobody read an owner-fenced entry")
	}
}

// A tag predicate (a premium policy) is applied too: a forbidden tag reads as absent
// and Find still fills its limit from the rows the reader may see.
func TestEnforcingLedger_ForbiddenTagAndLimit(t *testing.T) {
	l := ledgerFixture()
	ctx := domain.WithScope(context.Background(), &domain.TagPredicate{ForbiddenTags: []string{"hr"}, Owners: []string{"alice"}})
	if e, _ := l.Get(ctx, "led_hr"); e != nil {
		t.Fatal("forbidden tag readable")
	}
	rows, err := l.Find(ctx, domain.LedgerQuery{RunID: "r", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].ID != "led_alice" || rows[1].ID != "led_open" {
		t.Fatalf("rows = %+v, want alice and open", rows)
	}
}

// ADR-0145 D11 run-scope grant (T-20261112-staged-planner-s-ledger-reads-return-found):
// a predicate carrying LedgerRuns admits the entries of the named runs (every
// stage and plan generation) under the run's kernel scope — the owner fence for
// the run's owner only — and nothing else, by id or by query.
func TestEnforcingLedger_RunGrant(t *testing.T) {
	alice := domain.OwnerTag("alice")
	l := NewEnforcingLedger(&memLedger{rows: []domain.LedgerEntry{
		{ID: "led_own", RunID: "aa11", Tags: []string{"worker:out", alice}},
		{ID: "led_own_s2", RunID: "aa11-s2", Tags: []string{"worker:out", alice}},
		{ID: "led_own_gen", RunID: "aa11-s2-g1", Tags: []string{alice}},
		{ID: "led_other_run", RunID: "bb22", Tags: []string{"worker:out", alice}},
		{ID: "led_bob_in_run", RunID: "aa11", Tags: []string{domain.OwnerTag("bob")}},
		{ID: "led_bob_source", RunID: "aa11", Tags: []string{alice}, SourceTags: [][]string{{domain.OwnerTag("bob")}}},
		{ID: "led_no_run", Tags: []string{alice}},
	}}, nil)
	// A policy-shaped caller (not a bypass): it admits only "chat:*" material and
	// acts for nobody (no Owners), as a chat agent delegating without a header lease.
	caller := domain.TagPredicate{AnyOfClauses: [][]string{{"chat:general"}}}
	granted := caller
	granted.LedgerRuns = &domain.LedgerRunGrant{Roots: []string{"aa11"}, Owner: "alice"}
	ctx := domain.WithScope(context.Background(), &granted)

	for _, id := range []string{"led_own", "led_own_s2", "led_own_gen"} {
		if e, err := l.Get(ctx, id); err != nil || e == nil {
			t.Errorf("granted run entry %s not readable: %v", id, err)
		}
	}
	for _, id := range []string{"led_other_run", "led_bob_in_run", "led_bob_source", "led_no_run"} {
		if e, err := l.Get(ctx, id); err != nil || e != nil {
			t.Errorf("%s readable under the grant: %+v %v", id, e, err)
		}
	}
	rows, err := l.Find(ctx, domain.LedgerQuery{TurnID: "t"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range rows {
		got[e.ID] = true
	}
	if len(got) != 3 || !got["led_own"] || !got["led_own_s2"] || !got["led_own_gen"] {
		t.Errorf("Find under the grant = %v, want exactly the three own-run entries", got)
	}
	// Without the grant the same caller reads none of them (the bug).
	if e, _ := l.Get(domain.WithScope(context.Background(), &caller), "led_own"); e != nil {
		t.Error("the caller predicate alone read an owner-private run entry")
	}
	// An empty grant (no roots, or a root of "") grants nothing.
	empty := caller
	empty.LedgerRuns = &domain.LedgerRunGrant{Roots: []string{""}, Owner: "alice"}
	if e, _ := l.Get(domain.WithScope(context.Background(), &empty), "led_no_run"); e != nil {
		t.Error("an empty root granted an entry with no run")
	}
	// A grant with no owner admits no owner-scoped row, even in the granted run.
	noOwner := caller
	noOwner.LedgerRuns = &domain.LedgerRunGrant{Roots: []string{"aa11"}}
	if e, _ := l.Get(domain.WithScope(context.Background(), &noOwner), "led_own"); e != nil {
		t.Error("an ownerless grant read an owner-private entry")
	}
}
