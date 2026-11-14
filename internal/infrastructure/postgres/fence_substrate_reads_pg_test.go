package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// Cross-user privacy of every substrate READ a principal can reach, against a
// real migrated Postgres (PG_TEST_DSN; `orchestrator migrate up` at 1536 dims).
// T-20261107-substratelane-pointlookup-history,
// T-20261107-ask-knowledge-catalog-reads-entity-ids-and,
// T-20261107-link-review-inbox-and-entity-op-candidates-skip.
//
// The central property is stated as an EQUALITY, not as a list of things that
// must be absent: every answer bob and the operator plane (ScopeSystem) get —
// point, history, events, entity, alias expansion, the review inbox, and every
// catalog read (overview counts, sources and their tags, entities, predicates,
// event types, item kinds, actors) — is byte-identical before and after alice
// writes owner:alice rows that touch the very entities they ask about. Any
// difference at all is either a leaked value, a leaked name, or a hidden count.
// A list of forbidden strings would only catch the leaks someone thought of.
//
// The file name sorts before migration_guard_test.go on purpose (that test drops
// and recreates the public schema).

const (
	slShared     = "ownleak/shared"
	slAliceOnly  = "ownleak/alice-only"
	slAliasAlice = "ownleak/alias-alice"
	slAliasPub   = "ownleak/alias-pub"
	slCandAlice  = "ownleak/cand-alice"
	slCandPub    = "ownleak/cand-pub"
	slAliceType  = "ownleak_alice_meeting"
	slPubType    = "ownleak_public_type"
	slAlicePred  = "ownleak_secret_pred_alice"
	slAliceKind  = "ownleak_kind_alice"
	slAliceActor = "ownleak-actor-alice"
)

var (
	slAlice = &domain.TagPredicate{Owners: []string{"alice"}}
	slBob   = &domain.TagPredicate{Owners: []string{"bob"}}
)

type slFixture struct {
	pool   *pgxpool.Pool
	plane  *PgQueryPlane
	events *PgEventStore
	links  *PgLinkStore
	ctx    context.Context
	now    time.Time
}

func newSubstrateLeakFixture(t *testing.T) *slFixture {
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
	clean := func() {
		bg := context.Background()
		for _, q := range []string{
			`DELETE FROM links WHERE from_ref LIKE 'entity:ownleak/%' OR to_ref LIKE 'entity:ownleak/%'`,
			`DELETE FROM resolutions WHERE entity_id LIKE 'ownleak/%'`,
			`DELETE FROM knowledge_items WHERE entity_id LIKE 'ownleak/%'`,
			`DELETE FROM events WHERE source_ref LIKE 'ownleak-%'`,
			`DELETE FROM observations WHERE entity_id LIKE 'ownleak/%'`,
			`DELETE FROM entities WHERE id LIKE 'ownleak/%'`,
			`DELETE FROM evidence_outbox WHERE evidence_id LIKE 'ownleak-ev-%'`,
			`DELETE FROM evidence WHERE id LIKE 'ownleak-ev-%'`,
		} {
			if _, err := pool.Exec(bg, q); err != nil {
				t.Logf("cleanup %q: %v", q, err)
			}
		}
	}
	clean()
	t.Cleanup(clean)
	reg, err := domain.NewRelationRegistry(nil)
	if err != nil {
		t.Fatalf("relation registry: %v", err)
	}
	events := NewPgEventStore(pool, nil)
	links := NewPgLinkStore(pool, reg)
	return &slFixture{
		pool: pool, events: events, links: links, ctx: ctx,
		plane: NewPgQueryPlane(pool, events, NewPgKnowledgeStore(pool, nil), nil, links, NewPgEntityStore(pool, nil), reg),
		// Truncated: Postgres keeps microseconds, and a snapshot compares times.
		now: time.Now().UTC().Truncate(time.Second),
	}
}

func (f *slFixture) evidence(t *testing.T, id, source string, tags []string) domain.EvidenceID {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO evidence (id, namespace_id, source_id, source_key, content_hash, classification)
		VALUES ($1,'default',$2,$1,'sha256:test',$3)`, id, source, tags); err != nil {
		t.Fatalf("seed evidence %s: %v", id, err)
	}
	// An unprocessed outbox row, so the backlog figure has something to count.
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO evidence_outbox (evidence_id) VALUES ($1)`, id); err != nil {
		t.Fatalf("seed outbox %s: %v", id, err)
	}
	return domain.EvidenceID(id)
}

func (f *slFixture) observe(t *testing.T, ev domain.EvidenceID, entity, pred string, v float64, at time.Time) {
	t.Helper()
	if _, err := f.events.RecordObservation(f.ctx, domain.Observation{
		NamespaceID: "default", EntityID: entity, Predicate: pred,
		Value:      domain.StatementValue{Type: "number", Number: v},
		OccurredAt: at, Confidence: 1, EvidenceID: ev,
		SourceRef: fmt.Sprintf("ownleak-%s-%s-%s", ev, entity, pred),
	}); err != nil {
		t.Fatalf("observation %s/%s: %v", entity, pred, err)
	}
}

func (f *slFixture) event(t *testing.T, ev domain.EvidenceID, typ string, at time.Time, entities ...string) {
	t.Helper()
	e := domain.Event{NamespaceID: "default", Type: typ, OccurredAt: at, EvidenceID: ev,
		SourceRef: "ownleak-" + string(ev) + "-" + typ}
	for i, id := range entities {
		e.Roles = append(e.Roles, domain.EventRole{Role: fmt.Sprintf("r%d", i), EntityID: id})
	}
	if _, _, err := f.events.RecordEvent(f.ctx, e); err != nil {
		t.Fatalf("event %s: %v", typ, err)
	}
}

// item writes one knowledge item and its live resolution directly: the
// knowledge store's write path is not what is under test, and these tests must
// not depend on its resolution rules.
func (f *slFixture) item(t *testing.T, id, kind, entity, actor string, tags []string) {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO knowledge_items (id, namespace_id, kind, entity_id, asserted_by, asserted_at, classification)
		VALUES ($1,'default',$2,$3,$4,$5,$6)`, id, kind, entity, actor, f.now, tags); err != nil {
		t.Fatalf("item %s: %v", id, err)
	}
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO resolutions (namespace_id, kind, entity_id, actor, policy, item_id)
		VALUES ('default',$1,$2,$3,$4,$5)`, kind, entity, actor, domain.ResolutionPolicyLatestAssertion, id); err != nil {
		t.Fatalf("resolution %s: %v", id, err)
	}
}

func (f *slFixture) link(t *testing.T, from, to, state, mechanism string, ev domain.EvidenceID) {
	t.Helper()
	if _, err := f.links.PutLink(f.ctx, domain.Link{
		NamespaceID: "default", Family: domain.LinkFamilyIdentity,
		FromRef: domain.EntityRef(from), ToRef: domain.EntityRef(to),
		Relation: domain.RelationSameAs, State: state, Mechanism: mechanism,
		EvidenceID: ev, AssertedBy: "test", Producer: "ownleak@1",
		SourceRef: "ownleak-" + from + "-" + to,
	}); err != nil {
		t.Fatalf("link %s→%s: %v", from, to, err)
	}
}

// seedShared is everything that is NOT alice's: the public corpus and bob's own
// personal rows.
func (f *slFixture) seedShared(t *testing.T) {
	pub := f.evidence(t, "ownleak-ev-pub", "ownleak-src-pub", []string{"mail"})
	bob := f.evidence(t, "ownleak-ev-bob", "ownleak-src-bob", []string{"mail", "owner:bob"})
	f.observe(t, pub, slShared, "status", 1, f.now.Add(-3*time.Hour))
	f.observe(t, bob, slShared, "bob_note", 7, f.now.Add(-2*time.Hour))
	f.event(t, pub, slPubType, f.now.Add(-3*time.Hour), slShared)
	f.item(t, "ownleak-ki-pub", "ownleak_kind_pub", slShared, "ownleak-actor-pub", []string{"mail"})
	f.link(t, slShared, slAliasPub, domain.LinkStateConfirmed, domain.LinkMechanismDeclared, pub)
	f.observe(t, pub, slAliasPub, "status", 5, f.now.Add(-3*time.Hour))
	f.link(t, slShared, slCandPub, domain.LinkStateCandidate, domain.LinkMechanismScored, pub)
}

// seedAlicePrivate is alice's personal material, and it deliberately touches the
// SHARED entity: a newer value for the same key, an event the shared entity took
// part in, an alias and a candidate of it. Each one is a way a fence could leak.
func (f *slFixture) seedAlicePrivate(t *testing.T) {
	a := f.evidence(t, "ownleak-ev-alice", "ownleak-src-alice", []string{"mail", "owner:alice"})
	f.observe(t, a, slShared, "status", 2, f.now.Add(-1*time.Hour)) // NEWER than the public value
	f.observe(t, a, slAliceOnly, slAlicePred, 42, f.now.Add(-1*time.Hour))
	f.event(t, a, slAliceType, f.now.Add(-1*time.Hour), slAliceOnly, slShared)
	f.item(t, "ownleak-ki-alice", slAliceKind, slAliceOnly, slAliceActor, []string{"mail", "owner:alice"})
	f.link(t, slShared, slAliasAlice, domain.LinkStateConfirmed, domain.LinkMechanismDeclared, a)
	f.observe(t, a, slAliasAlice, "status", 9, f.now.Add(-1*time.Hour))
	f.link(t, slShared, slCandAlice, domain.LinkStateCandidate, domain.LinkMechanismScored, a)
}

// snapshot runs every principal-reachable read path under scope and renders the
// answers canonically. Errors are part of the answer.
func (f *slFixture) snapshot(t *testing.T, scope *domain.TagPredicate) string {
	t.Helper()
	ctx := f.ctx
	var b strings.Builder
	put := func(label string, v any, err error) {
		fmt.Fprintf(&b, "%s: %v err=%v\n", label, v, err)
	}
	window := func(q domain.KnowledgeQuery) domain.KnowledgeQuery {
		q.From, q.To = f.now.Add(-24*time.Hour), f.now.Add(time.Hour)
		return q
	}
	for _, q := range []domain.KnowledgeQuery{
		{Kind: domain.QueryPoint, EntityID: slShared, Predicate: "status"},
		{Kind: domain.QueryPoint, EntityID: slAliceOnly, Predicate: slAlicePred},
		window(domain.KnowledgeQuery{Kind: domain.QueryHistory, EntityID: slShared, Predicate: "status"}),
		window(domain.KnowledgeQuery{Kind: domain.QueryHistory, EntityID: slAliceOnly, Predicate: slAlicePred}),
		window(domain.KnowledgeQuery{Kind: domain.QueryEvents, EntityID: slShared}),
		window(domain.KnowledgeQuery{Kind: domain.QueryEvents, EntityID: slAliceOnly}),
		{Kind: domain.QueryEntity, EntityID: slShared},
		{Kind: domain.QueryEntity, EntityID: slShared, ExpandAliases: true},
		{Kind: domain.QueryEntity, EntityID: slAliasAlice},
		{Kind: domain.QueryPoint, EntityID: slShared, Predicate: "status", ExpandAliases: true},
		{Kind: domain.QueryCurrent, ItemKind: slAliceKind},
	} {
		res, err := f.plane.Query(ctx, q, scope)
		put(fmt.Sprintf("query %s %s %s expand=%v", q.Kind, q.EntityID, q.Predicate, q.ExpandAliases),
			fmt.Sprintf("closure=%d guarantee=%q rows=%v", res.ClosureSize, res.Guarantee, res.Rows), err)
	}
	cands, err := f.plane.ReviewCandidates(ctx, "default", domain.MaxQueryLimit, scope)
	var refs []string
	for _, l := range cands {
		refs = append(refs, l.FromRef+"→"+l.ToRef)
	}
	sort.Strings(refs)
	put("review inbox", refs, err)

	cat := f.plane.Catalog(scope)
	ov, err := cat.Overview(ctx)
	put("overview", fmt.Sprintf("%+v", ov), err)
	for _, ef := range []domain.EntityFilter{
		{Substrings: []string{"ownleak"}, IncludeResolutions: true},
		{Substrings: []string{"alice"}, IncludeResolutions: true},
		{Predicate: slAlicePred},
		{Predicate: "status"},
		{EventType: slAliceType},
		{IDs: []string{slShared, slAliceOnly, slAliasAlice}},
	} {
		ents, err := cat.Entities(ctx, ef)
		put(fmt.Sprintf("entities %+v", ef), fmt.Sprintf("%+v", ents), err)
	}
	preds, err := cat.Predicates(ctx, domain.MaxQueryLimit)
	put("predicates", preds, err)
	types, err := cat.EventTypes(ctx, domain.MaxQueryLimit)
	put("event types", types, err)
	kinds, err := cat.ItemKinds(ctx, domain.MaxQueryLimit)
	put("item kinds", kinds, err)
	actors, err := cat.Actors(ctx, nil, domain.MaxQueryLimit)
	put("actors (all)", actors, err)
	actors, err = cat.Actors(ctx, []string{slAliceOnly, slShared}, domain.MaxQueryLimit)
	put("actors (named)", actors, err)
	return b.String()
}

// User A's owner rows are invisible to B and to the operator plane on every
// read path, and leave no trace in any count.
func TestPg_SubstrateReads_AlicesPrivateRowsChangeNothingForBobOrTheOperator(t *testing.T) {
	f := newSubstrateLeakFixture(t)
	f.seedShared(t)
	readers := []struct {
		name  string
		scope *domain.TagPredicate
	}{
		{"bob", slBob},
		{"operator plane (ScopeSystem)", domain.ScopeSystem},
		{"agent acting for nobody", &domain.TagPredicate{}},
	}
	before := map[string]string{}
	for _, r := range readers {
		before[r.name] = f.snapshot(t, r.scope)
	}
	f.seedAlicePrivate(t)
	for _, r := range readers {
		after := f.snapshot(t, r.scope)
		if after != before[r.name] {
			t.Errorf("%s: alice's owner-scoped rows changed this reader's answers.\n%s",
				r.name, diffLines(before[r.name], after))
		}
		for _, s := range []string{"alice", "owner:alice", slAliceActor, slAliceKind, slAlicePred, slAliceType} {
			if strings.Contains(after, s) && !strings.Contains(before[r.name], s) {
				t.Errorf("%s: answer names %q", r.name, s)
			}
		}
	}
}

// diffLines reports the lines that differ, for a readable failure.
func diffLines(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	var out strings.Builder
	for i := 0; i < len(al) || i < len(bl); i++ {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			fmt.Fprintf(&out, "- %s\n+ %s\n", x, y)
		}
	}
	return out.String()
}

// The point shape answers with the latest row the caller may READ. Before the
// fix it took the newest row and filtered it, so alice's newer private value
// made the operator's (and bob's) ordinary answer vanish — an existence oracle.
func TestPg_SubstrateReads_PointIsLatestVisibleRow(t *testing.T) {
	f := newSubstrateLeakFixture(t)
	f.seedShared(t)
	f.seedAlicePrivate(t)
	q := domain.KnowledgeQuery{Kind: domain.QueryPoint, EntityID: slShared, Predicate: "status"}
	for _, tc := range []struct {
		name  string
		scope *domain.TagPredicate
		want  float64
	}{
		{"alice sees her own newer value", slAlice, 2},
		{"bob sees the public value", slBob, 1},
		{"operator sees the public value", domain.ScopeSystem, 1},
		{"maintenance sees the newest of all", domain.ScopeMaintenance, 2},
	} {
		res, err := f.plane.Query(f.ctx, q, tc.scope)
		if err != nil || len(res.Rows) != 1 {
			t.Fatalf("%s: rows=%v err=%v", tc.name, res.Rows, err)
		}
		if got := res.Rows[0]["value_number"]; got != tc.want {
			t.Errorf("%s: value %v, want %v", tc.name, got, tc.want)
		}
	}
	// "Hidden" and "never existed" read the same: no row, no error.
	hidden, herr := f.plane.Query(f.ctx, domain.KnowledgeQuery{
		Kind: domain.QueryPoint, EntityID: slAliceOnly, Predicate: slAlicePred}, slBob)
	absent, aerr := f.plane.Query(f.ctx, domain.KnowledgeQuery{
		Kind: domain.QueryPoint, EntityID: "ownleak/never-minted", Predicate: slAlicePred}, slBob)
	if herr != nil || aerr != nil || len(hidden.Rows) != 0 || len(absent.Rows) != 0 ||
		hidden.Guarantee != absent.Guarantee {
		t.Fatalf("hidden vs absent differ: hidden=%+v/%v absent=%+v/%v", hidden, herr, absent, aerr)
	}
}

// A still sees their own rows, on every path the fence now covers.
func TestPg_SubstrateReads_AliceStillSeesHerOwnRows(t *testing.T) {
	f := newSubstrateLeakFixture(t)
	f.seedShared(t)
	f.seedAlicePrivate(t)
	ctx := f.ctx

	ev, err := f.plane.Query(ctx, domain.KnowledgeQuery{Kind: domain.QueryEvents, EntityID: slAliceOnly,
		From: f.now.Add(-24 * time.Hour), To: f.now.Add(time.Hour)}, slAlice)
	if err != nil || len(ev.Rows) != 1 {
		t.Errorf("alice's own event: rows=%v err=%v", ev.Rows, err)
	}
	hist, err := f.plane.Query(ctx, domain.KnowledgeQuery{Kind: domain.QueryHistory, EntityID: slShared,
		Predicate: "status", From: f.now.Add(-24 * time.Hour), To: f.now.Add(time.Hour)}, slAlice)
	if err != nil || len(hist.Rows) != 2 {
		t.Errorf("alice's history of the shared key (public + hers): rows=%v err=%v", hist.Rows, err)
	}
	// Alias expansion counts the alias only she can justify.
	exp, err := f.plane.Query(ctx, domain.KnowledgeQuery{Kind: domain.QueryPoint, EntityID: slShared,
		Predicate: "status", ExpandAliases: true}, slAlice)
	if err != nil || exp.ClosureSize != 2 || !strings.Contains(exp.Guarantee, "across 2 confirmed aliases") {
		t.Errorf("alice's closure: size=%d guarantee=%q err=%v", exp.ClosureSize, exp.Guarantee, err)
	}
	sys, err := f.plane.Query(ctx, domain.KnowledgeQuery{Kind: domain.QueryPoint, EntityID: slShared,
		Predicate: "status", ExpandAliases: true}, domain.ScopeSystem)
	if err != nil || sys.ClosureSize != 1 || !strings.Contains(sys.Guarantee, "across 1 confirmed aliases") {
		t.Errorf("operator's closure must count only the public alias: size=%d guarantee=%q err=%v",
			sys.ClosureSize, sys.Guarantee, err)
	}

	cat := f.plane.Catalog(slAlice)
	ents, err := cat.Entities(ctx, domain.EntityFilter{Predicate: slAlicePred})
	if err != nil || len(ents) != 1 || ents[0].ID != slAliceOnly || ents[0].ObservationCount != 1 {
		t.Errorf("alice's catalog: entities=%+v err=%v", ents, err)
	}
	if kinds, _ := cat.ItemKinds(ctx, 1000); !containsStr(kinds, slAliceKind) {
		t.Errorf("alice's item kinds lack her own: %v", kinds)
	}
	if actors, _ := cat.Actors(ctx, []string{slAliceOnly}, 10); len(actors) != 1 || actors[0] != slAliceActor {
		t.Errorf("alice's actors: %v", actors)
	}
	ov, err := cat.Overview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var sawOwn, sawBob bool
	for _, s := range ov.Sources {
		if s.SourceID == "ownleak-src-alice" && containsStr(s.Classification, "owner:alice") && s.EvidenceCount == 1 {
			sawOwn = true
		}
		if s.SourceID == "ownleak-src-bob" {
			sawBob = true
		}
	}
	if !sawOwn || sawBob {
		t.Errorf("alice's overview sources: own=%v bob's=%v (%+v)", sawOwn, sawBob, ov.Sources)
	}
	// Maintenance — the only unfenced scope — sees both candidates; the operator
	// plane's inbox sees only the public one.
	all, err := f.plane.ReviewCandidates(ctx, "default", 1000, domain.ScopeMaintenance)
	if err != nil || countRefs(all, slCandAlice) != 1 || countRefs(all, slCandPub) != 1 {
		t.Errorf("maintenance inbox: %v err=%v", all, err)
	}
	op, err := f.plane.ReviewCandidates(ctx, "default", 1000, domain.ScopeSystem)
	if err != nil || countRefs(op, slCandAlice) != 0 || countRefs(op, slCandPub) != 1 {
		t.Errorf("operator inbox: %v err=%v", op, err)
	}
	// ENG-818: the review WRITES are guarded by the same judgement — the operator
	// plane may act on the public candidate only; alice's answers as an absent id.
	for _, l := range all {
		alice := countRefs([]domain.Link{l}, slCandAlice) == 1
		if !alice && countRefs([]domain.Link{l}, slCandPub) != 1 {
			continue // another test's row in the shared database
		}
		ok, err := f.plane.LinkReadable(ctx, "default", l.ID, domain.ScopeSystem)
		if err != nil || ok == alice {
			t.Errorf("LinkReadable(%s, ScopeSystem) = %v err=%v (alice's candidate: %v)", l.ID, ok, err, alice)
		}
	}
	if ok, err := f.plane.LinkReadable(ctx, "default", "lnk_does_not_exist", domain.ScopeMaintenance); ok || err != nil {
		t.Errorf("LinkReadable(absent) = %v err=%v", ok, err)
	}
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func countRefs(links []domain.Link, entity string) int {
	n := 0
	for _, l := range links {
		if l.ToRef == domain.EntityRef(entity) || l.FromRef == domain.EntityRef(entity) {
			n++
		}
	}
	return n
}

// Nil scope denies on the new surfaces too (the QueryPlane rule).
func TestSubstrateCatalog_NilScopeDenies(t *testing.T) {
	p := &PgQueryPlane{}
	if _, err := p.Catalog(nil).Overview(context.Background()); !errors.Is(err, domain.ErrQueryScopeMissing) {
		t.Fatalf("catalog with nil scope: %v", err)
	}
	if _, err := p.ReviewCandidates(context.Background(), "default", 10, nil); !errors.Is(err, domain.ErrQueryScopeMissing) {
		t.Fatalf("review inbox with nil scope: %v", err)
	}
}
