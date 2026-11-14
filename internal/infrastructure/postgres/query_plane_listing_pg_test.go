package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// The catalog listings (Links, Events, EntityHandles;
// T-20261113-link-building-suite-reads-the-kernel-s-postgres) are fenced like every
// other catalog read: nil scope denies, an empty namespace is refused, and the
// operator plane's ScopeSystem never lists a row resting on alice's owner:alice
// evidence — while paging one row at a time reaches exactly the rows one big page
// returns.

func TestSubstrateListings_RefuseNilScopeAndEmptyNamespace(t *testing.T) {
	p := &PgQueryPlane{}
	ctx := context.Background()
	if _, err := p.Catalog(nil).Links(ctx, domain.LinkListFilter{Namespace: "default"}); !errors.Is(err, domain.ErrQueryScopeMissing) {
		t.Fatalf("links with nil scope: %v", err)
	}
	if _, err := p.Catalog(nil).Events(ctx, domain.EventListFilter{Namespace: "default"}); !errors.Is(err, domain.ErrQueryScopeMissing) {
		t.Fatalf("events with nil scope: %v", err)
	}
	if _, err := p.Catalog(nil).EntityHandles(ctx, domain.EntityHandleFilter{Namespace: "default"}); !errors.Is(err, domain.ErrQueryScopeMissing) {
		t.Fatalf("entity handles with nil scope: %v", err)
	}
}

func TestPg_SubstrateListings_FencedAndPaged(t *testing.T) {
	f := newSubstrateLeakFixture(t)
	f.seedShared(t)
	f.seedAlicePrivate(t)
	for _, e := range []struct{ id, ev string }{
		{slShared, "ownleak-ev-pub"}, {slAliasPub, "ownleak-ev-pub"}, {slAliceOnly, "ownleak-ev-alice"},
	} {
		if _, err := f.pool.Exec(f.ctx, `INSERT INTO entities (id, namespace_id, kind, first_seen_evidence)
			VALUES ($1,'default','ownleak',$2) ON CONFLICT DO NOTHING`, e.id, e.ev); err != nil {
			t.Fatalf("seed entity %s: %v", e.id, err)
		}
	}
	cat := f.plane.Catalog(domain.ScopeSystem)

	if _, err := cat.Links(f.ctx, domain.LinkListFilter{}); !errors.Is(err, errListingNamespace) {
		t.Fatalf("links without a namespace: %v", err)
	}

	// Links: all pages of one row equal one page; nothing of alice's.
	var paged []string
	after := ""
	for i := 0; i < 1000; i++ {
		pg, err := cat.Links(f.ctx, domain.LinkListFilter{Namespace: "default", ProducerPrefix: "ownleak@", AfterID: after, Limit: 1})
		if err != nil {
			t.Fatalf("links page: %v", err)
		}
		for _, l := range pg.Links {
			paged = append(paged, l.ToRef)
		}
		if pg.Next == "" {
			break
		}
		after = pg.Next
	}
	one, err := cat.Links(f.ctx, domain.LinkListFilter{Namespace: "default", ProducerPrefix: "ownleak@", Limit: 100})
	if err != nil {
		t.Fatalf("links: %v", err)
	}
	if len(one.Links) != len(paged) || len(one.Links) != 2 {
		t.Fatalf("paged %v vs one page %d links; want the 2 public links", paged, len(one.Links))
	}
	for _, l := range one.Links {
		if strings.Contains(l.ToRef, "alice") || strings.Contains(l.FromRef, "alice") {
			t.Fatalf("operator plane listed alice's link %+v", l)
		}
	}
	conf, err := cat.Links(f.ctx, domain.LinkListFilter{Namespace: "default", ProducerPrefix: "ownleak@",
		States: []string{domain.LinkStateConfirmed}, Limit: 100})
	// Identity links are stored canonically ordered (from_ref < to_ref,
	// 0016_entities_links.sql), so the seeded shared→alias-pub link reads back as
	// alias-pub→shared: compare the pair, not the direction.
	if err != nil || len(conf.Links) != 1 || !sameLinkPair(conf.Links[0], slShared, slAliasPub) {
		t.Fatalf("confirmed-only listing: %+v err=%v", conf.Links, err)
	}

	// Events by source_ref prefix: the public event only.
	evs, err := cat.Events(f.ctx, domain.EventListFilter{Namespace: "default", SourceRefPrefix: "ownleak-", Limit: 100})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(evs.Events) != 1 || evs.Events[0].EventType != slPubType {
		t.Fatalf("events listing = %+v, want only the public event", evs.Events)
	}

	// Entity handles: the two public handles, never alice's.
	hs, err := cat.EntityHandles(f.ctx, domain.EntityHandleFilter{Namespace: "default", AfterID: "ownleak/", Limit: 100})
	if err != nil {
		t.Fatalf("entity handles: %v", err)
	}
	var ids []string
	for _, h := range hs.Handles {
		if strings.HasPrefix(h.ID, "ownleak/") {
			ids = append(ids, h.ID)
		}
	}
	if strings.Join(ids, ",") != slAliasPub+","+slShared {
		t.Fatalf("entity handles = %v, want the two public handles", ids)
	}
}

// sameLinkPair reports whether l joins entities a and b in either direction.
func sameLinkPair(l domain.Link, a, b string) bool {
	ra, rb := domain.EntityRef(a), domain.EntityRef(b)
	return (l.FromRef == ra && l.ToRef == rb) || (l.FromRef == rb && l.ToRef == ra)
}
