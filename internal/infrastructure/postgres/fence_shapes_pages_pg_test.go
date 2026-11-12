package postgres

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// Filter-before-limit for the NON-walk shapes and entity's own link reads
// (T-20261112-substrate-query-shapes-apply-limit-before-the,
// T-20261112-query-plane-as-of-applies-sql-limit-before-the), and the set-based
// traverse (T-20261112-traverse-still-reads-events-once-per-frontier).
//
// Every fixture puts the rows a narrow caller may NOT read first in the order
// the shape pages in, so a page cut before the scope is made entirely of hidden
// rows. The full-scope answers are pinned by TestPg_NonWalkShapes_FullScope-
// AnswersUnchanged; here a narrow caller must get a FULL page of the rows it may
// read. The file name sorts before migration_guard_test.go on purpose.

func seCol(rows []domain.QueryRow, key string, f *seFixture) []string {
	var out []string
	for _, r := range rows {
		out = append(out, f.renderValue(r[key]))
	}
	return out
}

// as_of: resolutions page by (entity, actor), and shapeq/k0 — first — is all
// secret. Cut first, the page was empty.
func TestPg_NonWalkShapes_NarrowScopeGetsFullPages_AsOf(t *testing.T) {
	f := newShapeEqFixture(t)
	rows := f.query(t, domain.KnowledgeQuery{Kind: domain.QueryAsOf, ItemKind: seKind, AsOf: f.asOf, Limit: 2}, f.narrow)
	if got, want := seCol(rows, "item_id", f), []string{"I:k1a", "I:k1b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("narrow as_of limit 2 = %v, want a full page %v", got, want)
	}
	for _, r := range rows {
		if r["values"] == nil {
			t.Fatalf("an as_of row lost its values: %v", r)
		}
	}
}

// as_of no longer reads each item through a store call whose error it
// swallowed: a knowledge port that fails every GetItem used to turn a narrow
// caller's whole answer into "nothing" — a transient error indistinguishable
// from an empty record.
type failingItems struct{ domain.KnowledgeStore }

func (failingItems) GetItem(context.Context, domain.KnowledgeItemID) (*domain.KnowledgeItem, error) {
	return nil, errors.New("injected: item read failed")
}

func TestPg_NonWalkShapes_AsOfDoesNotDropRowsOnItemReadErrors(t *testing.T) {
	f := newShapeEqFixture(t)
	plane := NewPgQueryPlane(f.pool, f.events, failingItems{f.knowledge}, nil, f.links, NewPgEntityStore(f.pool), f.reg)
	res, err := plane.Query(f.ctx, domain.KnowledgeQuery{Kind: domain.QueryAsOf, ItemKind: seKind, AsOf: f.asOf}, f.narrow)
	if err != nil {
		t.Fatalf("as_of: %v", err)
	}
	if got, want := seCol(res.Rows, "item_id", f), []string{"I:k1a", "I:k1b", "I:k2b", "I:k3b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("as_of with a failing item port = %v, want %v", got, want)
	}
	// And a failure the shape DOES depend on fails the query rather than
	// emptying it.
	cancelled, cancel := context.WithCancel(f.ctx)
	cancel()
	if _, err := f.plane.Query(cancelled, domain.KnowledgeQuery{Kind: domain.QueryAsOf, ItemKind: seKind, AsOf: f.asOf}, f.narrow); err == nil {
		t.Fatal("as_of on a failed read answered instead of failing")
	}
}

// history and events page oldest first, and the two oldest rows are secret.
// Pushed into SQL, the fence must still hide them and the LIMIT must count
// readable rows only; the operator control proves the hidden rows are there.
func TestPg_NonWalkShapes_NarrowScopeGetsFullPages_HistoryAndEvents(t *testing.T) {
	f := newShapeEqFixture(t)
	from, to := f.base.Add(-48*time.Hour), f.base.Add(time.Hour)
	hist := func(scope *domain.TagPredicate) []string {
		return seCol(f.query(t, domain.KnowledgeQuery{Kind: domain.QueryHistory, EntityID: "shapeq/m1",
			Predicate: "temp", From: from, To: to, Limit: 2}, scope), "evidence_id", f)
	}
	if got, want := hist(f.narrow), []string{"EV:pub", "EV:pub"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("narrow history limit 2 evidence = %v, want %v", got, want)
	}
	if got, want := hist(domain.ScopeMaintenance), []string{"EV:secret", "EV:secret"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("maintenance history limit 2 evidence = %v, want %v", got, want)
	}
	evs := func(scope *domain.TagPredicate) []string {
		return seCol(f.query(t, domain.KnowledgeQuery{Kind: domain.QueryEvents, EntityID: "shapeq/c1",
			From: from, To: to, Limit: 2}, scope), "event_id", f)
	}
	if got, want := evs(f.narrow), []string{"E:e3", "E:e5"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("narrow events limit 2 = %v, want %v", got, want)
	}
	if got, want := evs(domain.ScopeMaintenance), []string{"E:e1", "E:e2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("maintenance events limit 2 = %v, want %v", got, want)
	}
	// alice reads her owner:alice rows and nobody else's: the own-rows fence
	// survives the move into SQL.
	alice := &domain.TagPredicate{Bypass: true, Owners: []string{"alice"}}
	bob := &domain.TagPredicate{Bypass: true, Owners: []string{"bob"}}
	if got := seCol(f.query(t, domain.KnowledgeQuery{Kind: domain.QueryEvents, EntityID: "shapeq/c1",
		From: from, To: to}, alice), "event_id", f); !strings.Contains(strings.Join(got, ","), "E:e4") {
		t.Fatalf("alice lost her own event: %v", got)
	}
	if got := seCol(f.query(t, domain.KnowledgeQuery{Kind: domain.QueryEvents, EntityID: "shapeq/c1",
		From: from, To: to}, bob), "event_id", f); strings.Contains(strings.Join(got, ","), "E:e4") {
		t.Fatalf("bob read alice's owner:alice event: %v", got)
	}
}

// entity: a ref's links come newest first and the newest are secret, in both
// lanes. Cut first, the outgoing lane of c1 and the incoming lane of c2 were
// empty for a narrow caller.
func TestPg_NonWalkShapes_NarrowScopeGetsFullPages_EntityLinks(t *testing.T) {
	f := newShapeEqFixture(t)
	links := func(entity string, limit int, scope *domain.TagPredicate) []string {
		var out []string
		for _, r := range f.query(t, domain.KnowledgeQuery{Kind: domain.QueryEntity, EntityID: entity, Limit: limit}, scope) {
			if r["row"] == "link" {
				out = append(out, fmt.Sprint(r["link_id"]))
			}
		}
		return out
	}
	if got, want := links("shapeq/c1", 3, f.narrow), []string{"shapeq-out-pub2", "shapeq-out-pub1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("narrow entity c1 limit 3 links = %v, want %v", got, want)
	}
	// An entity cited only from outside: its whole card is the incoming lane.
	c2 := domain.EntityRef("shapeq/c2")
	if _, err := NewPgEntityStore(f.pool).EnsureEntity(f.ctx, domain.Entity{
		NamespaceID: "default", ID: "shapeq/c2", FirstSeenEvidence: f.pub,
	}); err != nil {
		t.Fatalf("entity: %v", err)
	}
	for i, l := range []struct {
		name string
		from string
		ev   domain.EvidenceID
	}{{"c2-in-pub", f.ev["e3"], f.pub}, {"c2-in-sec1", f.ev["e1"], f.sec}, {"c2-in-sec2", f.ev["e2"], f.sec}} {
		if _, err := f.links.PutLink(f.ctx, domain.Link{
			ID: "shapeq-" + l.name, NamespaceID: "default", FromRef: l.from, ToRef: c2,
			Relation: "shapeq_cites", State: domain.LinkStateConfirmed,
			Mechanism: domain.LinkMechanismReference, EvidenceID: l.ev, AssertedBy: "test",
			Producer: "shapeq@1", Confidence: 1, SourceRef: "shapeq-" + l.name,
			AssertedAt: f.base.Add(time.Hour + time.Duration(i)*time.Minute),
		}); err != nil {
			t.Fatalf("link %s: %v", l.name, err)
		}
	}
	if got, want := links("shapeq/c2", 2, f.narrow), []string{"shapeq-c2-in-pub"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("narrow entity c2 limit 2 links = %v, want %v", got, want)
	}
	if got := links("shapeq/c2", 0, domain.ScopeMaintenance); len(got) != 3 {
		t.Fatalf("maintenance c2 links = %v, want all 3", got)
	}
}

// Through the principal seam an agent's or operator's query takes
// (authz.QueryKnowledgeFunc; AllowAllAuthorizer → bob reads at Bypass +
// owner:bob): the first resolutions in order are alice's owner:alice rows.
func TestPg_NonWalkShapes_AsOfFullPageThroughTheSeam(t *testing.T) {
	f := newShapeEqFixture(t)
	for i, actor := range []string{"a@x", "b@x"} {
		if _, _, err := f.knowledge.PutItem(f.ctx, domain.KnowledgeItem{
			NamespaceID: "default", Kind: seKind, EntityID: "shapeq/a0", AssertedBy: actor,
			AssertedAt: f.base.Add(time.Duration(i) * time.Minute), SourceRef: "shapeq-a0-" + actor,
			Classification: []string{"mail", "owner:alice"},
			Values:         []domain.StatementValue{{Predicate: "status", Type: "text", Text: "private"}},
		}); err != nil {
			t.Fatalf("item: %v", err)
		}
	}
	query := authz.QueryKnowledgeFunc(domain.AllowAllAuthorizer{}, f.plane, nil)
	res, err := query(f.ctx, domain.PrincipalRef{Kind: domain.PrincipalUser, ID: "bob"},
		domain.KnowledgeQuery{Kind: domain.QueryAsOf, ItemKind: seKind, AsOf: f.asOf, Limit: 2})
	if err != nil {
		t.Fatalf("bob as_of: %v", err)
	}
	if got, want := seCol(res.Rows, "item_id", f), []string{"I:k0a", "I:k0b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("bob as_of limit 2 = %v, want %v (alice's rows hidden, a full page of his)", got, want)
	}
}

// traverse reads a whole frontier per hop: widening hop 2's frontier from 2 to
// 8 entities must not add a single statement.
func TestPg_Traverse_OneRoundTripPerHop(t *testing.T) {
	f := newShapeEqFixture(t)
	count := func(width int) int64 {
		hub := fmt.Sprintf("shapeq/hub%d", width)
		mk := func(name string, hoursAgo int, ents ...string) {
			e := domain.Event{ID: domain.EventID("shapeq-evt-" + name), NamespaceID: "default", Type: "shapeq_rt",
				OccurredAt: f.base.Add(-time.Duration(hoursAgo) * time.Hour), EvidenceID: f.pub, SourceRef: "shapeq-" + name}
			for _, id := range ents {
				e.Roles = append(e.Roles, domain.EventRole{Role: "party", EntityID: id})
			}
			if _, _, err := f.events.RecordEvent(f.ctx, e); err != nil {
				t.Fatalf("event %s: %v", name, err)
			}
		}
		for i := 1; i <= width; i++ {
			leaf := fmt.Sprintf("shapeq/leaf%d-%d", width, i)
			mk(fmt.Sprintf("rt%d-mid%d", width, i), 2, hub, leaf)
			mk(fmt.Sprintf("rt%d-leaf%d", width, i), 3, leaf, fmt.Sprintf("shapeq/far%d-%d", width, i))
		}
		before := f.tracer.n.Load()
		res, err := f.plane.Query(f.ctx, domain.KnowledgeQuery{Kind: domain.QueryTraverse, EntityID: hub, Hops: 2,
			From: f.base.Add(-48 * time.Hour), To: f.base.Add(time.Hour)}, f.narrow)
		if err != nil {
			t.Fatalf("traverse: %v", err)
		}
		if len(res.Rows) != 2*width {
			t.Fatalf("width %d: got %d rows, want %d", width, len(res.Rows), 2*width)
		}
		return f.tracer.n.Load() - before
	}
	narrow, wide := count(2), count(8)
	if narrow != wide {
		t.Fatalf("round trips grew with the frontier: width 2 = %d statements, width 8 = %d", narrow, wide)
	}
	t.Logf("traverse, 2 hops: %d statements at width 2 and at width 8", narrow)
}

// EventsForEntities is EventsForEntity per entity — same events, order and
// roles, retired rows excluded — and an event shared by two asked entities
// comes back under both.
func TestPg_EventsForEntities_MatchesEventsForEntity(t *testing.T) {
	f := newShapeEqFixture(t)
	from, to := f.base.Add(-48*time.Hour), f.base.Add(time.Hour)
	ids := []string{"shapeq/c1", "shapeq/o3", "shapeq/none"}
	set, err := f.events.EventsForEntities(f.ctx, "default", ids, from, to)
	if err != nil {
		t.Fatalf("set read: %v", err)
	}
	for _, id := range ids {
		one, err := f.events.EventsForEntity(f.ctx, "default", id, from, to)
		if err != nil {
			t.Fatalf("one read %s: %v", id, err)
		}
		if !reflect.DeepEqual(one, set[id]) {
			t.Fatalf("%s: set read %+v != single read %+v", id, set[id], one)
		}
		for _, ev := range set[id] {
			if ev.SourceRef == "shapeq-e6" {
				t.Fatalf("%s: retired event e6 returned", id)
			}
		}
	}
	if len(set["shapeq/c1"]) != 5 || len(set["shapeq/o3"]) != 2 || len(set["shapeq/none"]) != 0 {
		t.Fatalf("counts c1=%d o3=%d none=%d, want 5/2/0", len(set["shapeq/c1"]), len(set["shapeq/o3"]), len(set["shapeq/none"]))
	}
	// e3 has c1 and o3 as participants: under both keys, roles hydrated.
	for _, id := range []string{"shapeq/c1", "shapeq/o3"} {
		found := false
		for _, ev := range set[id] {
			if ev.SourceRef == "shapeq-e3" && len(ev.Roles) == 3 {
				found = true
			}
		}
		if !found {
			t.Fatalf("shared event e3 missing (or roles not hydrated) under %s: %+v", id, set[id])
		}
	}
}
