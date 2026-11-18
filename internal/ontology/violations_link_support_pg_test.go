package ontology

import (
	"reflect"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
)

// ENG-2049 (owner decision 2026-11-18, option a): the violations report fences a
// link on EVERY evidence row it rests on (sqlfence.LinkExists). Every link here
// cites evidence both readers may read and carries no row classification; the
// supported ones also rest on evidence restricted by a policy tag (hr). A reader
// barred from that evidence is told nothing built on a supported link and is
// told what its own view implies (order/5, whose only link is supported, holds
// no readable link); a granted reader sees them. Links with no supporting
// evidence read exactly as before (order/2's plain link).
func TestPg_Violations_SupportingEvidenceNarrowsTheReport(t *testing.T) {
	pool, ctx := pgPool(t)
	const ns = "onto-test-linksupport"
	cleanNamespace(t, pool, ns)
	o, err := Load(ctx, pool, seedDoc(), authorities())
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range []struct{ id, tags string }{{"onto-ls-ev", "{team:a}"}, {"onto-ls-ev-hr", "{team:a,hr}"}} {
		if _, err := pool.Exec(ctx, `INSERT INTO evidence (id, namespace_id, source_id, source_key, content_hash, classification)
			VALUES ($1,$2,'test',$1,'sha256:test',$3::text[])`, ev.id, ns, ev.tags); err != nil {
			t.Fatal(err)
		}
	}
	const ev = domain.EvidenceID("onto-ls-ev")
	support := []domain.EvidenceID{"onto-ls-ev-hr"}
	entities := postgres.NewPgEntityStore(pool, o)
	for _, e := range []string{"order/2", "order/5"} {
		if _, err := entities.EnsureEntity(ctx, domain.Entity{ID: e, NamespaceID: ns, FirstSeenEvidence: ev}); err != nil {
			t.Fatal(err)
		}
	}
	links := postgres.NewPgLinkStore(pool, o)
	put := func(from, to string, supp []domain.EvidenceID) {
		t.Helper()
		if _, err := links.PutLink(ctx, domain.Link{NamespaceID: ns, Relation: "placed_by", FromRef: from, ToRef: to,
			State: domain.LinkStateConfirmed, Mechanism: domain.LinkMechanismRecord, Producer: "test@1", EvidenceID: ev,
			AssertedBy: "test", SourceRef: from + ">" + to, SupportingEvidence: supp}); err != nil {
			t.Fatalf("link %s→%s: %v", from, to, err)
		}
	}
	put("entity:invoice/9", "entity:customer/1", support) // from_kind, supported
	put("entity:order/2", "entity:customer/1", nil)       // cardinality: one plain ...
	put("entity:order/2", "entity:customer/2", support)   // ... and one supported target
	put("entity:order/5", "entity:customer/3", support)   // order/5's only link is supported

	if res, err := o.Apply(ctx, ns, domain.OntologyDoc{
		Relations: []domain.RelationSpec{{Name: "placed_by", Family: domain.LinkFamilyRelation,
			FromKind: "order", ToKind: "customer", Cardinality: domain.CardinalityOne, Required: true, Inverse: "placed"}},
	}, "user:op", false); err != nil || !res.Applied {
		t.Fatalf("declare: %+v %v", res, err)
	}
	report := func(scope *domain.TagPredicate) map[string][]string {
		t.Helper()
		vs, _, err := o.Violations(ctx, scope, ns, 0)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string][]string{}
		for _, rule := range []string{domain.ViolationFromKind, domain.ViolationCardinality, domain.ViolationRequiredLink} {
			if got := rulesFor(vs, rule); got != nil {
				out[rule] = got
			}
		}
		return out
	}
	granted := report(&domain.TagPredicate{})
	if len(granted[domain.ViolationFromKind]) != 1 || len(granted[domain.ViolationCardinality]) != 1 ||
		granted[domain.ViolationRequiredLink] != nil {
		t.Errorf("granted report = %v, want one from_kind, one cardinality, no required-link violation", granted)
	}
	closed := report(&domain.TagPredicate{ForbiddenTags: []string{"hr"}})
	if want := map[string][]string{domain.ViolationRequiredLink: {"order/5"}}; !reflect.DeepEqual(closed, want) {
		t.Errorf("hr-closed report = %v, want %v", closed, want)
	}
	// With the supporting sets emptied the two readers agree (narrow-only).
	if _, err := pool.Exec(ctx, `UPDATE links SET supporting_evidence = '{}' WHERE namespace_id = $1`, ns); err != nil {
		t.Fatal(err)
	}
	if a, b := report(&domain.TagPredicate{}), report(&domain.TagPredicate{ForbiddenTags: []string{"hr"}}); !reflect.DeepEqual(a, b) || !reflect.DeepEqual(a, granted) {
		t.Errorf("without supporting evidence: granted %v, hr-closed %v, want both %v", a, b, granted)
	}
}
