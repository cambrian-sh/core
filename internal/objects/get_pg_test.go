package objects_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// Deterministic integration tests of the read side's rules, one rule each
// (the randomized differential is list_pg_test.go).

var (
	southOnly = &domain.TagPredicate{AnyOfClauses: [][]string{{"r:south", "r:global"}}, ForbiddenTags: []string{"cls:restricted"},
		PartyScopedTags: []string{"cls:confidential"}, PartyIdentities: []string{"am:1"}}
	northOnly = &domain.TagPredicate{AnyOfClauses: [][]string{{"r:north"}}}
)

func ids(rows []domain.ObjectRow) []string {
	out := []string{}
	for _, r := range rows {
		out = append(out, r.EntityID)
	}
	return out
}

func (e *lpEnv) list(t *testing.T, p *domain.TagPredicate, q domain.ObjectsQuery) domain.ObjectsResult {
	t.Helper()
	q.Namespace = e.ns
	res, err := e.reader.List(e.ctx, p, q)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// A property whose value the caller cannot read is invisible in values,
// filters, sorts, groups, metrics and existence.
func TestPg_HiddenPropertyIsAbsentEverywhere(t *testing.T) {
	e := newLPEnv(t, "lptest-hidden")
	ev := e.seedEvidence("hid-")
	_ = ev
	e.mustObserve("lporder/o1", "total", num(100), 0, "hid-north", "a1") // only north may see
	e.mustObserve("lporder/o1", "status", txt("open"), 0, "hid-south", "a2")
	e.mustObserve("lporder/o2", "total", num(5), 0, "hid-south", "b1")
	e.mustObserve("lporder/o3", "total", num(50), 0, "hid-restr", "c1") // restricted: nobody south

	// values: o1 shows status, not total.
	res := e.list(t, southOnly, domain.ObjectsQuery{Kind: lpOrder})
	if got := renderRows(res.Rows); fmt.Sprint(got) != "[lporder/o1{status=t:open} lporder/o2{total=n:5.000000}]" {
		t.Fatalf("values: %v", got)
	}
	// filter: total > 10 matches nothing south may see (o1's 100 is hidden, o3 restricted).
	if r := e.list(t, southOnly, domain.ObjectsQuery{Kind: lpOrder, Where: []domain.ObjectCondition{
		{Property: "total", Op: domain.ObjectOpGt, Values: []domain.StatementValue{num(10)}}}}); len(r.Rows) != 0 {
		t.Fatalf("filter on a hidden value matched: %v", ids(r.Rows))
	}
	// is_null: a hidden value reads as absent.
	if r := e.list(t, southOnly, domain.ObjectsQuery{Kind: lpOrder, Where: []domain.ObjectCondition{
		{Property: "total", Op: domain.ObjectOpIsNull}}}); fmt.Sprint(ids(r.Rows)) != "[lporder/o1]" {
		t.Fatalf("is_null: %v", ids(r.Rows))
	}
	// sort: total desc puts o2 (5) first and o1 (hidden = absent) last.
	if r := e.list(t, southOnly, domain.ObjectsQuery{Kind: lpOrder, OrderBy: []domain.ObjectOrder{{Property: "total", Desc: true}}}); fmt.Sprint(ids(r.Rows)) != "[lporder/o2 lporder/o1]" {
		t.Fatalf("sort: %v", ids(r.Rows))
	}
	// metrics: the sum south sees is 5; north sees 100; maintenance 155.
	for _, c := range []struct {
		p    *domain.TagPredicate
		want string
	}{{southOnly, "[ => n:2.000000,n:5.000000]"}, {northOnly, "[ => n:1.000000,n:100.000000]"}, {domain.ScopeMaintenance, "[ => n:3.000000,n:155.000000]"}} {
		r := e.list(t, c.p, domain.ObjectsQuery{Kind: lpOrder, Metrics: []domain.ObjectMetric{{Fn: "count"}, {Fn: "sum", Property: "total"}}})
		if got := fmt.Sprint(renderGroups(r.Groups)); got != c.want {
			t.Fatalf("metrics under %+v: %v, want %v", *c.p, got, c.want)
		}
	}
	// existence: o3 is not listed for south, and GetObject says not found —
	// the same answer as for an id that never existed.
	for _, id := range []string{"lporder/o3", "lporder/never"} {
		if _, err := e.reader.Get(e.ctx, southOnly, e.ns, id); !errors.Is(err, domain.ErrObjectNotFound) {
			t.Fatalf("get %s: %v", id, err)
		}
	}
	// ...until the caller can read the evidence that minted it (ADR-0128 D12).
	if _, err := e.entities.EnsureEntity(e.ctx, domain.Entity{ID: "lporder/o3", NamespaceID: e.ns, FirstSeenEvidence: "hid-global"}); err != nil {
		t.Fatal(err)
	}
	v, err := e.reader.Get(e.ctx, southOnly, e.ns, "lporder/o3")
	if err != nil || len(v.Properties) != 0 || v.FirstSeenEvidence != "hid-global" || v.CreatedAt == nil {
		t.Fatalf("minted by readable evidence: %+v %v", v, err)
	}
	if r := e.list(t, southOnly, domain.ObjectsQuery{Kind: lpOrder}); fmt.Sprint(ids(r.Rows)) != "[lporder/o1 lporder/o2 lporder/o3]" {
		t.Fatalf("list after mint: %v", ids(r.Rows))
	}
}

// GetObject: provenance, per-caller conflict alternatives, typed links in both
// directions with the inverse label, aliases, timeline, lifecycle.
func TestPg_GetObjectPage(t *testing.T) {
	e := newLPEnv(t, "lptest-get")
	e.seedEvidence("get-")
	e.mustObserve("lporder/o1", "status", txt("open"), 0, "get-south", "s1")
	e.mustObserve("lporder/o1", "status", txt("picking"), time.Hour, "get-north", "s2") // newer, other source: wins, conflict
	e.mustObserve("lporder/o1", "total", num(42), 0, "get-south", "t1")
	e.mustObserve("lpcust/c1", "segment", txt("hospital"), 0, "get-south", "c1")
	if _, err := e.links.PutLink(e.ctx, domain.Link{NamespaceID: e.ns, FromRef: "entity:lporder/o1", ToRef: "entity:lpcust/c1",
		Relation: lpVerb, State: domain.LinkStateConfirmed, Mechanism: domain.LinkMechanismRecord, EvidenceID: "get-south",
		AssertedBy: "t", AssertedAt: base, SourceRef: "l1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO events (id, namespace_id, event_type, occurred_at, evidence_id, source_ref) VALUES
		('lpget-ev1', $1, 'shipped', $2, 'get-south', 'e1'), ('lpget-ev2', $1, 'audited', $2, 'get-restr', 'e2')`, e.ns, base.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO event_roles VALUES ('lpget-ev1','order','lporder/o1'), ('lpget-ev2','order','lporder/o1')`); err != nil {
		t.Fatal(err)
	}

	// Maintenance: the winner is north's picking, with south's open as an alternative.
	v, err := e.reader.Get(e.ctx, domain.ScopeMaintenance, e.ns, "lporder/o1")
	if err != nil {
		t.Fatal(err)
	}
	st := v.Properties[0]
	if st.Property != "status" || st.Value.Text != "picking" || st.Source != "crm" || !st.Conflict ||
		len(st.Alternatives) != 1 || st.Alternatives[0].Value.Text != "open" || st.Alternatives[0].Source != "erp" {
		t.Fatalf("status: %+v", st)
	}
	if len(v.Links) != 1 || v.Links[0].Direction != domain.ObjectLinkOut || v.Links[0].OtherKind != lpCust || v.Links[0].Label != lpVerb {
		t.Fatalf("links: %+v", v.Links)
	}
	if len(v.Timeline) != 5 || v.Timeline[0].Kind != domain.ObjectTimelineEvent { // 2 events + 3 observations, newest first
		t.Fatalf("timeline: %+v", v.Timeline)
	}

	// South: the winning status is north's, hidden — so status is ABSENT (an
	// older visible value never stands in), and no conflict is reported.
	v, err = e.reader.Get(e.ctx, southOnly, e.ns, "lporder/o1")
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Properties) != 1 || v.Properties[0].Property != "total" || v.Properties[0].Conflict {
		t.Fatalf("south properties: %+v", v.Properties)
	}
	for _, en := range v.Timeline {
		if en.EventID == "lpget-ev2" || (en.Property == "status" && en.Value.Text == "picking") {
			t.Fatalf("south sees a hidden timeline entry: %+v", en)
		}
	}
	// North: sees picking, but south's open is not readable — no conflict.
	v, err = e.reader.Get(e.ctx, northOnly, e.ns, "lporder/o1")
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Properties) != 1 || v.Properties[0].Value.Text != "picking" || v.Properties[0].Conflict || len(v.Links) != 0 {
		t.Fatalf("north: %+v", v)
	}
	// The customer page shows the link incoming, under its declared inverse.
	v, err = e.reader.Get(e.ctx, southOnly, e.ns, "lpcust/c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Links) != 1 || v.Links[0].Direction != domain.ObjectLinkIn || v.Links[0].Label != "lp_has_order" ||
		v.Links[0].OtherRef != "entity:lporder/o1" || v.Links[0].OtherKind != lpOrder {
		t.Fatalf("incoming link: %+v", v.Links)
	}
	if fmt.Sprint(v.Evidence) != "[get-south]" {
		t.Fatalf("evidence: %v", v.Evidence)
	}
}

func TestPg_GetObjectLifecycle(t *testing.T) {
	pool, ctx := pgPool(t)
	_ = ctx
	e := newLPEnv(t, "lptest-life")
	_ = pool
	// A kind with a lifecycle, declared live through Apply.
	life := domain.KindSpec{Kind: "lpcase", Table: true, Predicates: map[string]domain.ValueSpec{"state": {Type: "text"}},
		Lifecycle: &domain.KindLifecycle{StatePredicate: "state", Initial: []string{"new"},
			Transitions: map[string][]string{"new": {"triage"}, "triage": {"closed"}}}}
	if _, err := e.onto.Apply(e.ctx, e.ns, domain.OntologyDoc{Kinds: []domain.KindSpec{life}}, "test", false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = e.pool.Exec(e.ctx, `DROP TABLE IF EXISTS objects_lpcase`) }) // test-only
	e.seedEvidence("life-")
	e.mustObserve("lpcase/k1", "state", txt("triage"), 0, "life-south", "k1")
	v, err := e.reader.Get(e.ctx, southOnly, e.ns, "lpcase/k1")
	if err != nil {
		t.Fatal(err)
	}
	if v.Lifecycle == nil || v.Lifecycle.State != "triage" || fmt.Sprint(v.Lifecycle.Allowed) != "[closed]" {
		t.Fatalf("lifecycle: %+v", v.Lifecycle)
	}
}

// Keyset paging is stable under concurrent inserts: rows that existed before
// the first page are each returned exactly once, in order, whatever is written
// between pages.
func TestPg_KeysetStableUnderConcurrentInserts(t *testing.T) {
	e := newLPEnv(t, "lptest-keyset")
	e.seedEvidence("ks-")
	for i := 0; i < 20; i++ {
		e.mustObserve(fmt.Sprintf("lporder/k%02d", i), "total", num(float64(i%7)), 0, "ks-south", fmt.Sprintf("k%d", i))
	}
	q := domain.ObjectsQuery{Kind: lpOrder, Namespace: e.ns, OrderBy: []domain.ObjectOrder{{Property: "total"}}, PageSize: 3}
	seen := map[string]int{}
	var order []string
	for page := 0; page < 50; page++ {
		res, err := e.reader.List(e.ctx, southOnly, q)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range res.Rows {
			seen[r.EntityID]++
			order = append(order, r.EntityID)
		}
		// Between pages: new objects at every sort position, and a value change.
		e.mustObserve(fmt.Sprintf("lporder/n%02d", page), "total", num(float64(page%7)), 0, "ks-south", fmt.Sprintf("n%d", page))
		if res.NextPageToken == "" {
			break
		}
		q.PageToken = res.NextPageToken
	}
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("lporder/k%02d", i)
		if seen[id] != 1 {
			t.Fatalf("%s returned %d times (order %v)", id, seen[id], order)
		}
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("%s returned %d times", id, n)
		}
	}
	// And a token is refused under changed parameters.
	q.OrderBy[0].Desc = true
	if _, err := e.reader.List(e.ctx, southOnly, q); !errors.Is(err, domain.ErrObjectsInvalid) {
		t.Fatalf("changed request accepted a token: %v", err)
	}
}

// The reader refuses a nil predicate before any SQL (fail closed).
func TestPg_NilScopeRefused(t *testing.T) {
	e := newLPEnv(t, "lptest-nil")
	if _, err := e.reader.List(e.ctx, nil, domain.ObjectsQuery{Kind: lpOrder}); !errors.Is(err, domain.ErrQueryScopeMissing) {
		t.Fatal(err)
	}
	if _, err := e.reader.Get(e.ctx, nil, e.ns, "lporder/o1"); !errors.Is(err, domain.ErrQueryScopeMissing) {
		t.Fatal(err)
	}
}
