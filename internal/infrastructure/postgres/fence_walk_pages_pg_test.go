package postgres

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// Filter before limit, and one round trip per lane per hop, in the graph-walk
// shapes (T-20261112-substrate-query-shapes-apply-limit-before-the,
// T-20261112-why-traverse-issue-one-query-per-frontier-node).
//
// Each fixture puts the rows a narrow caller may NOT read first in the store's
// order, so a page cut before the scope is applied is made entirely of hidden
// rows. Before the rewrite such a caller got an empty answer while readable
// rows existed; now it gets a full page of them. The operator (ScopeMaintenance)
// answer is the control that the readable rows are really there.
//
// The file name sorts before migration_guard_test.go on purpose (that test drops
// and recreates the public schema).

// countingTracer counts the statements a pool sends, so "one query per hop"
// is measured against the engine rather than inferred from the code.
type countingTracer struct{ n atomic.Int64 }

func (c *countingTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}

func (c *countingTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

type wpFixture struct {
	pool   *pgxpool.Pool
	tracer *countingTracer
	plane  *PgQueryPlane
	events *PgEventStore
	links  *PgLinkStore
	evs    *PgEvidenceStore
	ctx    context.Context
	base   time.Time
	pub    domain.EvidenceID
	sec    domain.EvidenceID
	narrow *domain.TagPredicate
}

func newWalkPageFixture(t *testing.T) *wpFixture {
	t.Helper()
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping integration test that requires PostgreSQL")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	tracer := &countingTracer{}
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	clean := func() {
		bg := context.Background()
		for _, q := range []string{
			`DELETE FROM links WHERE source_ref LIKE 'walkpg-%'`,
			`DELETE FROM event_roles WHERE entity_id LIKE 'walkpg/%'`,
			`DELETE FROM events WHERE source_ref LIKE 'walkpg-%'`,
			`DELETE FROM evidence_outbox WHERE evidence_id IN (SELECT id FROM evidence WHERE source_id = 'walkpg-src')`,
			`DELETE FROM evidence WHERE source_id = 'walkpg-src'`,
		} {
			if _, err := pool.Exec(bg, q); err != nil {
				t.Logf("cleanup %q: %v", q, err)
			}
		}
	}
	clean()
	t.Cleanup(clean)
	reg, err := domain.NewRelationRegistry([]domain.RelationSpec{
		{Name: "walkpg_caused_by", Family: domain.LinkFamilyLineage},
	})
	if err != nil {
		t.Fatalf("relation registry: %v", err)
	}
	f := &wpFixture{
		pool: pool, tracer: tracer, ctx: ctx,
		events: NewPgEventStore(pool, nil), links: NewPgLinkStore(pool, reg),
		evs:    NewPgEvidenceStore(pool),
		base:   time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC),
		narrow: &domain.TagPredicate{RequiredTags: []string{"mail"}, ForbiddenTags: []string{"k-secret"}},
	}
	f.plane = NewPgQueryPlane(pool, f.events, nil, nil, f.links, NewPgEntityStore(pool), reg)
	f.pub = f.evidence(t, "pub", []string{"mail"})
	f.sec = f.evidence(t, "secret", []string{"mail", "k-secret"})
	return f
}

func (f *wpFixture) evidence(t *testing.T, name string, tags []string) domain.EvidenceID {
	t.Helper()
	id, _, err := f.evs.Insert(f.ctx, domain.Evidence{
		NamespaceID: "default", SourceID: "walkpg-src", SourceKey: name,
		SourceTime: f.base, IngestedAt: f.base,
		ContentHash: domain.CID("sha256:walkpg-" + name), Classification: tags,
	})
	if err != nil {
		t.Fatalf("evidence %s: %v", name, err)
	}
	return id
}

func (f *wpFixture) event(t *testing.T, name string, ev domain.EvidenceID, minutesAgo int, entities ...string) string {
	t.Helper()
	e := domain.Event{ID: domain.EventID("walkpg-evt-" + name), NamespaceID: "default",
		Type: "walkpg_" + name, OccurredAt: f.base.Add(-time.Duration(minutesAgo) * time.Minute),
		EvidenceID: ev, SourceRef: "walkpg-" + name}
	for _, id := range entities {
		e.Roles = append(e.Roles, domain.EventRole{Role: "party", EntityID: id})
	}
	if _, _, err := f.events.RecordEvent(f.ctx, e); err != nil {
		t.Fatalf("event %s: %v", name, err)
	}
	return domain.RefPrefixEvent + "walkpg-evt-" + name
}

func (f *wpFixture) why(t *testing.T, ref string, hops, limit int, scope *domain.TagPredicate) []domain.QueryRow {
	t.Helper()
	res, err := f.plane.Query(f.ctx, domain.KnowledgeQuery{Kind: domain.QueryWhy, IncludeCorrelation: true, EntityID: ref, Hops: hops, Limit: limit}, scope)
	if err != nil {
		t.Fatalf("why %s: %v", ref, err)
	}
	return res.Rows
}

func hopTargets(rows []domain.QueryRow) []string {
	var out []string
	for _, r := range rows {
		if r["row"] == "hop" || r["row"] == "related" {
			out = append(out, fmt.Sprint(r["to_ref"]))
		}
	}
	return out
}

// The event-anchored co-occurrence lane: the three NEWEST neighbours are
// secret, so a limit-2 page cut before the scope is all hidden.
func TestPg_WalkShapes_NarrowScopeGetsFullPages_SharedObjectLane(t *testing.T) {
	f := newWalkPageFixture(t)
	root := f.event(t, "root", f.pub, 0, "walkpg/x")
	for i := 1; i <= 3; i++ {
		f.event(t, fmt.Sprintf("hidden%d", i), f.sec, i, "walkpg/x")
	}
	vis1 := f.event(t, "visible1", f.pub, 10, "walkpg/x")
	vis2 := f.event(t, "visible2", f.pub, 11, "walkpg/x")

	got := hopTargets(f.why(t, root, 1, 2, f.narrow))
	if strings.Join(got, ",") != vis1+","+vis2 {
		t.Fatalf("narrow caller got %v, want the two readable neighbours [%s %s]", got, vis1, vis2)
	}
	// Control: the operator sees the hidden ones first, so the page really was
	// full of rows the narrow caller may not read.
	if all := hopTargets(f.why(t, root, 1, 2, domain.ScopeMaintenance)); len(all) != 2 || all[0] == vis1 {
		t.Fatalf("the operator's first page should be the secret neighbours, got %v", all)
	}
}

// The stored-lineage lane: the three most recently asserted links rest on
// secret evidence.
func TestPg_WalkShapes_NarrowScopeGetsFullPages_LinkLane(t *testing.T) {
	f := newWalkPageFixture(t)
	root := f.event(t, "lroot", f.pub, 0)
	var visible []string
	for i := 1; i <= 5; i++ {
		target := f.event(t, fmt.Sprintf("ltarget%d", i), f.pub, i)
		ev := f.pub
		if i > 2 {
			ev = f.sec // the three asserted LAST, i.e. first in the store's order
		} else {
			visible = append([]string{target}, visible...)
		}
		if _, err := f.links.PutLink(f.ctx, domain.Link{
			ID: fmt.Sprintf("walkpg-l%d", i), NamespaceID: "default", FromRef: root, ToRef: target,
			Relation: "walkpg_caused_by", State: domain.LinkStateConfirmed,
			Mechanism: domain.LinkMechanismReference, EvidenceID: ev, AssertedBy: "test",
			Producer: "walkpg@1", Confidence: 1, SourceRef: fmt.Sprintf("walkpg-l%d", i),
			AssertedAt: f.base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("link %d: %v", i, err)
		}
	}
	got := hopTargets(f.why(t, root, 1, 2, f.narrow))
	if strings.Join(got, ",") != strings.Join(visible, ",") {
		t.Fatalf("narrow caller got %v, want the two readable links' targets %v", got, visible)
	}
}

// The entity-anchored pair lane, asked the way a user asks: through the
// principal seam (AllowAllAuthorizer, so bob reads at Bypass + owner:bob) with
// the newest pairs resting on alice's owner:alice evidence.
func TestPg_WalkShapes_NarrowScopeGetsFullPages_EntityLaneThroughTheSeam(t *testing.T) {
	f := newWalkPageFixture(t)
	ali := f.evidence(t, "alice", []string{"mail", "owner:alice"})
	// Pairs are ordered newest-earlier-event first: two alice events at the
	// newest end make every pair in a limit-2 page involve one of them.
	f.event(t, "e-old1", f.pub, 50, "walkpg/y")
	f.event(t, "e-old2", f.pub, 40, "walkpg/y")
	f.event(t, "e-old3", f.pub, 30, "walkpg/y")
	f.event(t, "e-alice1", ali, 2, "walkpg/y")
	f.event(t, "e-alice2", ali, 1, "walkpg/y")
	query := authz.QueryKnowledgeFunc(domain.AllowAllAuthorizer{}, f.plane, nil)
	res, err := query(f.ctx, domain.PrincipalRef{Kind: domain.PrincipalUser, ID: "bob"},
		domain.KnowledgeQuery{Kind: domain.QueryWhy, IncludeCorrelation: true, EntityID: "walkpg/y", Hops: 1, Limit: 2})
	if err != nil {
		t.Fatalf("bob why: %v", err)
	}
	got := hopTargets(res.Rows)
	if len(got) != 2 {
		t.Fatalf("bob got %d pairs (%v), want a full page of 2 readable pairs", len(got), res.Rows)
	}
	for _, r := range res.Rows {
		if strings.Contains(fmt.Sprint(r), "e-alice") {
			t.Fatalf("a pair involving alice's event reached bob: %v", r)
		}
	}
}

// The identity closure: more hidden aliases than the store's page, all newer
// than the one alias the caller may read.
func TestPg_WalkShapes_NarrowScopeGetsFullPages_IdentityClosure(t *testing.T) {
	f := newWalkPageFixture(t)
	subject := domain.EntityRef("walkpg/subject")
	put := func(i int, to string, ev domain.EvidenceID, at time.Time) {
		if _, err := f.links.PutLink(f.ctx, domain.Link{
			NamespaceID: "default", FromRef: subject, ToRef: domain.EntityRef(to),
			Relation: domain.RelationSameAs, State: domain.LinkStateConfirmed,
			Mechanism: domain.LinkMechanismDeclared, EvidenceID: ev, AssertedBy: "test",
			Producer: "walkpg@1", Confidence: 1, SourceRef: fmt.Sprintf("walkpg-alias-%d", i),
			AssertedAt: at,
		}); err != nil {
			t.Fatalf("alias link %d: %v", i, err)
		}
	}
	put(0, "walkpg/visible-alias", f.pub, f.base)
	for i := 1; i <= defaultIdentityLimit+5; i++ {
		put(i, fmt.Sprintf("walkpg/hidden-alias-%03d", i), f.sec, f.base.Add(time.Duration(i)*time.Second))
	}
	got, err := f.plane.identityClosure(f.ctx, "walkpg/subject", 1, f.narrow)
	if err != nil {
		t.Fatalf("closure: %v", err)
	}
	if len(got.members) != 1 || got.members[0].entityID != "walkpg/visible-alias" {
		t.Fatalf("narrow closure = %+v, want exactly the readable alias", got.members)
	}
	if len(got.flagged) != 0 {
		t.Fatalf("hidden links were counted by the fan-out guard: %v", got.flagged)
	}
}

// One statement per lane per HOP, not per frontier node: widening the frontier
// from 2 to 8 events must not add a single round trip.
func TestPg_WalkShapes_OneRoundTripPerLanePerHop(t *testing.T) {
	f := newWalkPageFixture(t)
	count := func(width int) int64 {
		hub := fmt.Sprintf("walkpg/hub%d", width)
		root := f.event(t, fmt.Sprintf("hubroot%d", width), f.pub, 0, hub)
		for i := 1; i <= width; i++ {
			// Each neighbour has its own earlier neighbour, so hop 2 has a
			// frontier of `width` events, each with something to find.
			leaf := fmt.Sprintf("walkpg/leaf%d-%d", width, i)
			f.event(t, fmt.Sprintf("mid%d-%d", width, i), f.pub, 10+i, hub, leaf)
			f.event(t, fmt.Sprintf("leaf%d-%d", width, i), f.pub, 100+i, leaf)
		}
		before := f.tracer.n.Load()
		rows := f.why(t, root, 2, 100, f.narrow)
		if len(rows) < 2*width {
			t.Fatalf("width %d: want at least %d hops, got %d", width, 2*width, len(rows))
		}
		return f.tracer.n.Load() - before
	}
	narrow, wide := count(2), count(8)
	if narrow != wide {
		t.Fatalf("round trips grew with the frontier: width 2 = %d statements, width 8 = %d", narrow, wide)
	}
	t.Logf("why, 2 hops: %d statements at width 2 and at width 8", narrow)
}
