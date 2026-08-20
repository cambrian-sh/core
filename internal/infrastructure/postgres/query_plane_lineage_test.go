package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// The two reads D-W5-6 adds: incoming lineage on the `entity` shape, and
// co-occurrence hops that `why` computes for an ENTITY ref rather than only for
// an event one.
//
// The first is a claim about which queries the shape issues, so it runs against
// a fake. The second is a JOIN over event_roles and can only be a claim about
// SQL, so it runs DSN-gated against a real engine — the newScopePlane
// convention in query_plane_identity_test.go.

// inboxLinkStore answers LinksFor from a table and reports what it was asked.
// Separate from fakeLinkStore because the closure's fake panics on Candidates
// deliberately, and the entity shape reads the review inbox for an operator.
type inboxLinkStore struct {
	byRef map[string][]domain.Link
	// asked records every LinkQuery, in order: the property under test is
	// which reads the shape ISSUES, and a fake that only returned rows would
	// let a shape pass by luck.
	asked []domain.LinkQuery
}

func (f *inboxLinkStore) LinksFor(_ context.Context, _, ref string, opts domain.LinkQuery) ([]domain.Link, error) {
	f.asked = append(f.asked, opts)
	var out []domain.Link
	for _, l := range f.byRef[ref] {
		if opts.Family != "" && l.Family != opts.Family {
			continue
		}
		if opts.State != "" && l.State != opts.State {
			continue
		}
		// The store returns a TO-side row only when the walk asked for the
		// backward direction (or the verb is symmetric, which none here is).
		if l.ToRef == ref && l.FromRef != ref && !opts.IncludeIncoming {
			continue
		}
		out = append(out, l)
	}
	return out, nil
}

func (f *inboxLinkStore) PutLink(context.Context, domain.Link) (bool, error) {
	panic("the entity shape must not write")
}

func (f *inboxLinkStore) ConfirmLink(context.Context, string, string, string) (domain.Link, error) {
	panic("the entity shape must not confirm")
}

func (f *inboxLinkStore) RetractLink(context.Context, string, string, string, string) error {
	panic("the entity shape must not retract")
}

func (f *inboxLinkStore) Candidates(context.Context, string, int) ([]domain.Link, error) {
	return nil, nil
}

func (f *inboxLinkStore) RetractByProducer(context.Context, string, string, string) (int, error) {
	panic("the entity shape must not revoke")
}

// A citation runs from the citing record TO the entity, so an entity that is
// referenced by everything asserts nothing itself — and its card was empty.
func TestEntityOp_ReturnsIncomingLineage(t *testing.T) {
	subject := domain.EntityRef("customer/C-1")
	citing := domain.EntityRef("ticket/OPS-1")
	incoming := domain.Link{
		ID: "lnk-cite", NamespaceID: defaultNamespace, Family: domain.LinkFamilyLineage,
		FromRef: citing, ToRef: subject, Relation: "referenced_by",
		State: domain.LinkStateConfirmed, Mechanism: domain.LinkMechanismReference,
		EvidenceID: "ev-cite", AssertedBy: "test", Confidence: 1,
	}
	outgoing := domain.Link{
		ID: "lnk-own", NamespaceID: defaultNamespace, Family: domain.LinkFamilyRelation,
		FromRef: subject, ToRef: domain.EntityRef("account/A-1"), Relation: "subsidiary_of",
		State: domain.LinkStateConfirmed, Mechanism: domain.LinkMechanismRecord,
		EvidenceID: "ev-own", AssertedBy: "test", Confidence: 1,
	}
	links := &inboxLinkStore{byRef: map[string][]domain.Link{
		subject: {outgoing, incoming},
		citing:  {incoming},
	}}
	reg, err := domain.NewRelationRegistry([]domain.RelationSpec{
		{Name: "referenced_by", Family: domain.LinkFamilyLineage},
		{Name: "subsidiary_of", Family: domain.LinkFamilyRelation},
	})
	if err != nil {
		t.Fatalf("relation registry: %v", err)
	}
	plane := NewPgQueryPlane(nil, nil, nil, nil, links, nil, reg)

	res, err := plane.Query(context.Background(),
		domain.KnowledgeQuery{Kind: domain.QueryEntity, EntityID: "customer/C-1"},
		&domain.TagPredicate{Bypass: true})
	if err != nil {
		t.Fatalf("entity op: %v", err)
	}

	var cited, owned int
	for _, row := range res.Rows {
		if row["row"] != "link" {
			continue
		}
		switch row["link_id"] {
		case "lnk-cite":
			cited++
			if row["direction"] != "incoming" {
				t.Errorf("a citation of the subject is not marked incoming: %v", row)
			}
		case "lnk-own":
			owned++
			if _, stamped := row["direction"]; stamped {
				t.Errorf("a link the subject asserted must carry no direction key: %v", row)
			}
		}
	}
	if cited != 1 {
		t.Fatalf("the incoming citation appeared %d times, want exactly once", cited)
	}
	if owned != 1 {
		t.Fatalf("the subject's own link appeared %d times, want exactly once", owned)
	}

	// The second read must be LINEAGE-scoped and backward. An unfiltered
	// backward read would drag in every relation somebody else asserted about
	// this handle, which belongs on their card and not on this one.
	var sawIncomingRead bool
	for _, q := range links.asked {
		if q.IncludeIncoming {
			sawIncomingRead = true
			if q.Family != domain.LinkFamilyLineage {
				t.Errorf("the backward read was not lineage-scoped: %+v", q)
			}
			if q.State != domain.LinkStateConfirmed {
				t.Errorf("the backward read admitted unconfirmed rows: %+v", q)
			}
		}
	}
	if !sawIncomingRead {
		t.Fatal("the entity shape never asked for incoming links, so this test proved nothing")
	}
}

// ─── why(entity:X), against a real database ──────────────────────────────────
//
// The lane needs an engine: it is a JOIN over event_roles that pairs the
// occurrences one entity took part in, and a fake answering it would be
// asserting the thing under test.

const k5SharedEntity = "k5why/shared"

func newLineagePlane(t *testing.T) (*PgQueryPlane, *pgxpool.Pool, context.Context) {
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
		_, _ = pool.Exec(bg, `DELETE FROM event_roles WHERE entity_id LIKE 'k5why/%'`)
		_, _ = pool.Exec(bg, `DELETE FROM events WHERE source_ref LIKE 'k5why-%'`)
		_, _ = pool.Exec(bg, `DELETE FROM links WHERE from_ref LIKE '%k5why/%' OR to_ref LIKE '%k5why/%'`)
		_, _ = pool.Exec(bg, `DELETE FROM entities WHERE id LIKE 'k5why/%'`)
		_, _ = pool.Exec(bg, `DELETE FROM evidence WHERE id LIKE 'k5why-ev-%'`)
	}
	clean()
	t.Cleanup(clean)

	reg, err := domain.NewRelationRegistry(nil)
	if err != nil {
		t.Fatalf("relation registry: %v", err)
	}
	events := NewPgEventStore(pool, nil)
	plane := NewPgQueryPlane(pool, events, nil, nil,
		NewPgLinkStore(pool, reg), NewPgEntityStore(pool), reg)
	return plane, pool, ctx
}

func seedK5Evidence(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string, tags []string) domain.EvidenceID {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO evidence (id, namespace_id, source_id, source_key, content_hash, classification)
		VALUES ($1,'default','k5why-source',$1,'sha256:test',$2)
		ON CONFLICT (id) DO NOTHING`, id, tags); err != nil {
		t.Fatalf("seed evidence %s: %v", id, err)
	}
	return domain.EvidenceID(id)
}

// The lane D-W5-6 adds: asked about an ENTITY, `why` finds the occurrences that
// entity holds together, with no stored link between them.
func TestPg_WhyFromEntityRef_ComputesSharedObjectHops(t *testing.T) {
	plane, pool, ctx := newLineagePlane(t)
	events := NewPgEventStore(pool, nil)
	ev := seedK5Evidence(t, ctx, pool, "k5why-ev-open", []string{"k5-open"})

	base := time.Now().UTC().Truncate(time.Second)
	for i, spec := range []struct {
		ref  string
		kind string
		at   time.Time
	}{
		{"k5why-order", "order_placed", base.Add(-2 * time.Hour)},
		{"k5why-ship", "shipment_dispatched", base.Add(-1 * time.Hour)},
	} {
		if _, _, err := events.RecordEvent(ctx, domain.Event{
			NamespaceID: "default", Type: spec.kind, OccurredAt: spec.at,
			EvidenceID: ev, SourceRef: spec.ref,
			Roles: []domain.EventRole{{Role: "subject", EntityID: k5SharedEntity}},
		}); err != nil {
			t.Fatalf("record event %d: %v", i, err)
		}
	}

	res, err := plane.Query(ctx,
		domain.KnowledgeQuery{Kind: domain.QueryWhy, EntityID: domain.EntityRef(k5SharedEntity)},
		&domain.TagPredicate{Bypass: true})
	if err != nil {
		t.Fatalf("why(entity): %v", err)
	}
	var hops int
	for _, row := range res.Rows {
		if row["row"] != "hop" {
			continue
		}
		hops++
		if row["mechanism"] != domain.LinkMechanismSharedObject {
			t.Errorf("a co-occurrence hop is not labelled shared_object: %v", row)
		}
		if row["relation"] != domain.RelationPrecededAndSharesEntities {
			t.Errorf("a co-occurrence hop names a verb it has no standing to: %v", row)
		}
		if row["stored"] != false {
			t.Errorf("a computed hop claimed to be stored: %v", row)
		}
		if row["shared_entity_id"] != k5SharedEntity {
			t.Errorf("the hop does not say WHAT the two events share: %v", row)
		}
		// The hop runs event → event; the entity is what they share, never an
		// endpoint. An "entity preceded event" hop would be a claim about a
		// handle that nothing did.
		from, _ := row["from_ref"].(string)
		to, _ := row["to_ref"].(string)
		if len(from) < 6 || from[:6] != "event:" || len(to) < 6 || to[:6] != "event:" {
			t.Errorf("a co-occurrence hop has a non-event endpoint: %v", row)
		}
	}
	if hops != 1 {
		t.Fatalf("want exactly one co-occurrence hop between the two events, got %d (rows %v)", hops, res.Rows)
	}
}

// The same walk under a scope: a hop is a claim about BOTH events, so a caller
// who may not read one of them is not told the other stands next to it.
func TestPg_WhyFromEntityRef_HopsRespectBothEndsScope(t *testing.T) {
	plane, pool, ctx := newLineagePlane(t)
	events := NewPgEventStore(pool, nil)
	openEv := seedK5Evidence(t, ctx, pool, "k5why-ev-open", []string{"k5-open"})
	secretEv := seedK5Evidence(t, ctx, pool, "k5why-ev-secret", []string{"k5-secret"})

	base := time.Now().UTC().Truncate(time.Second)
	if _, _, err := events.RecordEvent(ctx, domain.Event{
		NamespaceID: "default", Type: "order_placed", OccurredAt: base.Add(-2 * time.Hour),
		EvidenceID: openEv, SourceRef: "k5why-order",
		Roles: []domain.EventRole{{Role: "subject", EntityID: k5SharedEntity}},
	}); err != nil {
		t.Fatalf("record readable event: %v", err)
	}
	if _, _, err := events.RecordEvent(ctx, domain.Event{
		NamespaceID: "default", Type: "internal_review", OccurredAt: base.Add(-1 * time.Hour),
		EvidenceID: secretEv, SourceRef: "k5why-secret",
		Roles: []domain.EventRole{{Role: "subject", EntityID: k5SharedEntity}},
	}); err != nil {
		t.Fatalf("record forbidden event: %v", err)
	}

	q := domain.KnowledgeQuery{Kind: domain.QueryWhy, EntityID: domain.EntityRef(k5SharedEntity)}
	scoped, err := plane.Query(ctx, q, &domain.TagPredicate{RequiredTags: []string{"k5-open"}})
	if err != nil {
		t.Fatalf("scoped why: %v", err)
	}
	for _, row := range scoped.Rows {
		if row["row"] == "hop" {
			t.Fatalf("a co-occurrence hop exposed an event the caller cannot read: %v", row)
		}
	}
	// Non-vacuous: an operator sees the hop the stranger did not.
	all, err := plane.Query(ctx, q, &domain.TagPredicate{Bypass: true})
	if err != nil {
		t.Fatalf("operator why: %v", err)
	}
	var hops int
	for _, row := range all.Rows {
		if row["row"] == "hop" {
			hops++
		}
	}
	if hops == 0 {
		t.Fatal("the operator saw no hop either, so the stranger's empty answer proved nothing")
	}
}
