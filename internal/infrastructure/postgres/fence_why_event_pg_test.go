package postgres

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// `why` must decide the event it STANDS on before it expands it
// (T-20261112-query-plane-why-never-fences-the-root-event-or).
//
// The defect: why(event:X) went straight to the shared-object lane, which fenced
// only the NEIGHBOUR events, so a caller who may not read X still got rows iff X
// existed (an existence oracle, ADR-0128 D12) and every row carried X's own
// participant role. The same held for an event reached through a readable
// lineage link at hop >= 2.
//
// The central property is an EQUALITY: asking about a hidden event is
// byte-identical to asking about an id that was never written. Every fixture row
// is written through the real writers (PgEvidenceStore.Insert,
// PgEventStore.RecordEvent, PgLinkStore.PutLink), and the owner-fence cases are
// asked through the real principal seam (authz.QueryKnowledgeFunc over the OSS
// AllowAllAuthorizer) — the exact function app.go hands premium's substrate
// plugin as KernelServices.QueryKnowledge, which serves `why` to the published
// MCP query tool and the operator Query RPC.
//
// The file name sorts before migration_guard_test.go on purpose (that test drops
// and recreates the public schema).

const (
	wfShared     = "whyfence/order"
	wfAliceRole  = "whyfence_alice_private_role"
	wfSecretRole = "whyfence_secret_role"
	wfMadeUp     = "event:whyfence-never-written"
)

type wfFixture struct {
	pool     *pgxpool.Pool
	plane    *PgQueryPlane
	evidence *PgEvidenceStore
	events   *PgEventStore
	links    *PgLinkStore
	ctx      context.Context
	now      time.Time

	pubEarly, secret, alice, root string // event refs
}

func newWhyFenceFixture(t *testing.T) *wfFixture {
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
			`DELETE FROM links WHERE source_ref LIKE 'whyfence-%'`,
			`DELETE FROM event_roles WHERE entity_id LIKE 'whyfence/%'`,
			`DELETE FROM events WHERE source_ref LIKE 'whyfence-%'`,
			`DELETE FROM entities WHERE id LIKE 'whyfence/%'`,
			`DELETE FROM evidence_outbox WHERE evidence_id IN (SELECT id FROM evidence WHERE source_id = 'whyfence-src')`,
			`DELETE FROM evidence WHERE source_id = 'whyfence-src'`,
		} {
			if _, err := pool.Exec(bg, q); err != nil {
				t.Logf("cleanup %q: %v", q, err)
			}
		}
	}
	clean()
	t.Cleanup(clean)

	reg, err := domain.NewRelationRegistry([]domain.RelationSpec{
		{Name: "whyfence_caused_by", Family: domain.LinkFamilyLineage},
	})
	if err != nil {
		t.Fatalf("relation registry: %v", err)
	}
	events := NewPgEventStore(pool, nil)
	links := NewPgLinkStore(pool, reg)
	f := &wfFixture{
		pool: pool, evidence: NewPgEvidenceStore(pool), events: events, links: links, ctx: ctx,
		plane: NewPgQueryPlane(pool, events, NewPgKnowledgeStore(pool, nil), nil, links, NewPgEntityStore(pool, nil), reg),
		now:   time.Now().UTC().Truncate(time.Second),
	}
	f.seed(t)
	return f
}

func (f *wfFixture) ev(t *testing.T, key string, tags []string) domain.EvidenceID {
	t.Helper()
	id, _, err := f.evidence.Insert(f.ctx, domain.Evidence{
		NamespaceID: "default", SourceID: "whyfence-src", SourceKey: key,
		SourceTime: f.now, IngestedAt: f.now,
		ContentHash: domain.CID("sha256:whyfence-" + key), Classification: tags,
	})
	if err != nil {
		t.Fatalf("evidence %s: %v", key, err)
	}
	return id
}

func (f *wfFixture) event(t *testing.T, ev domain.EvidenceID, key, typ string, at time.Time, roles ...domain.EventRole) string {
	t.Helper()
	id, _, err := f.events.RecordEvent(f.ctx, domain.Event{
		NamespaceID: "default", Type: typ, OccurredAt: at, EvidenceID: ev,
		SourceRef: "whyfence-" + key, Roles: roles,
	})
	if err != nil {
		t.Fatalf("event %s: %v", key, err)
	}
	return domain.RefPrefixEvent + string(id)
}

// seed: a public order event; a later event only `k-secret` readers may see; a
// later event private to alice (owner:alice, the private-by-default stamp); and
// a public ROOT event whose only lineage is a READABLE stored link to alice's
// event — the hop >= 2 route onto a hidden event.
func (f *wfFixture) seed(t *testing.T) {
	pub := f.ev(t, "pub", []string{"mail"})
	sec := f.ev(t, "secret", []string{"mail", "k-secret"})
	ali := f.ev(t, "alice", []string{"mail", "owner:alice"})

	f.pubEarly = f.event(t, pub, "pub-early", "order_placed", f.now.Add(-3*time.Hour),
		domain.EventRole{Role: "subject", EntityID: wfShared})
	f.secret = f.event(t, sec, "secret", "whyfence_secret_review", f.now.Add(-2*time.Hour),
		domain.EventRole{Role: wfSecretRole, EntityID: wfShared})
	f.alice = f.event(t, ali, "alice", "whyfence_alice_meeting", f.now.Add(-1*time.Hour),
		domain.EventRole{Role: wfAliceRole, EntityID: wfShared})
	f.root = f.event(t, pub, "root", "whyfence_root", f.now.Add(-30*time.Minute))

	if _, err := f.links.PutLink(f.ctx, domain.Link{
		NamespaceID: "default", Family: domain.LinkFamilyLineage,
		FromRef: f.root, ToRef: f.alice, Relation: "whyfence_caused_by",
		State: domain.LinkStateConfirmed, Mechanism: domain.LinkMechanismReference,
		EvidenceID: pub, AssertedBy: "test", Producer: "whyfence@1", Confidence: 1,
		SourceRef: "whyfence-root-alice",
	}); err != nil {
		t.Fatalf("lineage link: %v", err)
	}
}

func (f *wfFixture) why(t *testing.T, ref string, scope *domain.TagPredicate) (domain.QueryResult, error) {
	t.Helper()
	return f.plane.Query(f.ctx, domain.KnowledgeQuery{Kind: domain.QueryWhy, IncludeCorrelation: true, EntityID: ref, Hops: 3}, scope)
}

// render is the canonical answer: rows and error together, so "equal answers"
// includes the error class.
func wfRender(res domain.QueryResult, err error) string {
	return fmt.Sprintf("%#v | err=%v", res, err)
}

func wfMentions(res domain.QueryResult, needle string) bool {
	return strings.Contains(fmt.Sprintf("%v", res.Rows), needle)
}

// A caller whose TAG scope excludes the event gets exactly the answer an absent
// id gets; a caller who may read it still gets the full walk.
func TestPg_WhyHiddenRootEvent_AnswersAsAnAbsentID(t *testing.T) {
	f := newWhyFenceFixture(t)
	scoped := &domain.TagPredicate{RequiredTags: []string{"mail"}, ForbiddenTags: []string{"k-secret"}}

	hidden, hErr := f.why(t, f.secret, scoped)
	absent, aErr := f.why(t, wfMadeUp, scoped)
	if hErr != nil || aErr != nil {
		t.Fatalf("errors: hidden=%v absent=%v", hErr, aErr)
	}
	if len(hidden.Rows) != 0 {
		t.Fatalf("why(hidden event) returned rows — an existence oracle: %v", hidden.Rows)
	}
	if wfMentions(hidden, wfSecretRole) {
		t.Fatalf("the hidden event's role leaked: %v", hidden.Rows)
	}
	// Byte-identical, modulo the asked id (which the result does not echo).
	if wfRender(hidden, hErr) != wfRender(absent, aErr) || !reflect.DeepEqual(hidden, absent) {
		t.Fatalf("hidden and absent ids answer differently:\n hidden: %s\n absent: %s",
			wfRender(hidden, hErr), wfRender(absent, aErr))
	}

	// The flag-OFF answer (stored lineage + the ADR-0148 hint) must not tell the
	// two apart either: the hint is computed only for a root the caller may read.
	offQ := func(ref string) (domain.QueryResult, error) {
		return f.plane.Query(f.ctx, domain.KnowledgeQuery{Kind: domain.QueryWhy, EntityID: ref, Hops: 3}, scoped)
	}
	offHidden, ohErr := offQ(f.secret)
	offAbsent, oaErr := offQ(wfMadeUp)
	if wfRender(offHidden, ohErr) != wfRender(offAbsent, oaErr) {
		t.Fatalf("flag-off: hidden and absent ids answer differently: hidden=%s absent=%s",
			wfRender(offHidden, ohErr), wfRender(offAbsent, oaErr))
	}
	// Non-vacuous: the same flag-off question about a READABLE event with
	// related events carries the hint.
	offOpen, err := f.plane.Query(f.ctx, domain.KnowledgeQuery{Kind: domain.QueryWhy, EntityID: f.pubEarly, Hops: 3},
		&domain.TagPredicate{RequiredTags: []string{"mail"}})
	if err != nil {
		t.Fatalf("flag-off why: %v", err)
	}
	_ = offOpen // pubEarly is the EARLIEST event: nothing precedes it, so no hint is due

	// Non-vacuous: a caller who may read the event gets the hop and its role.
	full, err := f.why(t, f.secret, &domain.TagPredicate{RequiredTags: []string{"mail"}})
	if err != nil {
		t.Fatalf("allowed why: %v", err)
	}
	if !wfHasHop(full, f.secret, f.pubEarly) || !wfMentions(full, wfSecretRole) {
		t.Fatalf("an allowed caller lost the walk: %v", full.Rows)
	}
	// ScopeMaintenance is unfiltered, as for every other shape.
	maint, err := f.why(t, f.secret, domain.ScopeMaintenance)
	if err != nil || !wfHasHop(maint, f.secret, f.pubEarly) {
		t.Fatalf("maintenance why lost the walk: %v %v", maint.Rows, err)
	}
}

// The owner fence, asked the way a user reaches it: a principal through the
// kernel's QueryKnowledge seam on the OSS default authorizer (Bypass + the
// reader's own owner). Bob asking about alice's private event is answered as if
// it never existed; alice gets her answer; the plain-Bypass system scope keeps
// the owner fence (as eventsFor does) and only ScopeMaintenance reads it.
func TestPg_WhyOwnerFencedRootEvent_ThroughTheQuerySeam(t *testing.T) {
	f := newWhyFenceFixture(t)
	query := authz.QueryKnowledgeFunc(domain.AllowAllAuthorizer{}, f.plane, nil)
	bob := domain.PrincipalRef{Kind: domain.PrincipalUser, ID: "bob"}
	alice := domain.PrincipalRef{Kind: domain.PrincipalUser, ID: "alice"}
	ask := func(p domain.PrincipalRef, ref string) (domain.QueryResult, error) {
		return query(f.ctx, p, domain.KnowledgeQuery{Kind: domain.QueryWhy, IncludeCorrelation: true, EntityID: ref, Hops: 3})
	}

	hidden, hErr := ask(bob, f.alice)
	absent, aErr := ask(bob, wfMadeUp)
	if wfRender(hidden, hErr) != wfRender(absent, aErr) {
		t.Fatalf("bob can tell alice's event exists:\n hidden: %s\n absent: %s",
			wfRender(hidden, hErr), wfRender(absent, aErr))
	}
	if wfMentions(hidden, wfAliceRole) {
		t.Fatalf("alice's private role leaked to bob: %v", hidden.Rows)
	}

	own, err := ask(alice, f.alice)
	if err != nil {
		t.Fatalf("alice why: %v", err)
	}
	if !wfHasHop(own, f.alice, f.pubEarly) || !wfMentions(own, wfAliceRole) {
		t.Fatalf("alice lost the walk from her own event: %v", own.Rows)
	}

	sys, sErr := f.why(t, f.alice, domain.ScopeSystem)
	sysAbsent, saErr := f.why(t, wfMadeUp, domain.ScopeSystem)
	if wfRender(sys, sErr) != wfRender(sysAbsent, saErr) {
		t.Fatalf("plain Bypass dropped the owner fence on the root event: %v", sys.Rows)
	}
	maint, err := f.why(t, f.alice, domain.ScopeMaintenance)
	if err != nil || !wfHasHop(maint, f.alice, f.pubEarly) {
		t.Fatalf("maintenance why lost alice's walk: %v %v", maint.Rows, err)
	}
}

// A hidden event reached through a READABLE stored lineage link at hop 1 is not
// expanded at hop 2: the link row itself is the link's own claim and stays, but
// nothing the walk would learn by standing on alice's event appears.
func TestPg_WhyHiddenEventReachedByLineage_IsNotExpanded(t *testing.T) {
	f := newWhyFenceFixture(t)
	query := authz.QueryKnowledgeFunc(domain.AllowAllAuthorizer{}, f.plane, nil)
	bob := domain.PrincipalRef{Kind: domain.PrincipalUser, ID: "bob"}

	res, err := query(f.ctx, bob, domain.KnowledgeQuery{Kind: domain.QueryWhy, IncludeCorrelation: true, EntityID: f.root, Hops: 3})
	if err != nil {
		t.Fatalf("bob why(root): %v", err)
	}
	for _, row := range res.Rows {
		if row["from_ref"] == f.alice {
			t.Fatalf("the walk expanded alice's hidden event at hop >= 2: %v", row)
		}
	}
	if wfMentions(res, wfAliceRole) {
		t.Fatalf("alice's private role leaked via lineage: %v", res.Rows)
	}

	// Non-vacuous: the maintenance walk does expand it.
	maint, err := f.why(t, f.root, domain.ScopeMaintenance)
	if err != nil {
		t.Fatalf("maintenance why(root): %v", err)
	}
	if !wfHasHop(maint, f.alice, f.pubEarly) {
		t.Fatalf("maintenance never reached alice's event, so bob's answer proved nothing: %v", maint.Rows)
	}
}

func wfHasHop(res domain.QueryResult, from, to string) bool {
	for _, row := range res.Rows {
		if (row["row"] == "hop" || row["row"] == "related") && row["from_ref"] == from && row["to_ref"] == to {
			return true
		}
	}
	return false
}
