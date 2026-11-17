package ontology

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
)

// ADR-0158 stage 3 (D3 sites 47-49): the violations report reads links and
// observations through the narrow-only typed-row fence. Every row here stands on
// evidence BOTH readers may read; only the row's own classification [hr] tells
// them apart. A reader for whom hr is closed is told nothing a restricted row
// shows — not a link-rule violation built on it, not a current value it holds —
// and is told what its own view implies (an entity whose only link it cannot
// read holds no readable link); a granted reader sees the restricted rows. For
// R = ∅ (every production row today) the two readers agree.
func TestPg_Violations_RowClassificationNarrowsTheReport(t *testing.T) {
	pool, ctx := pgPool(t)
	const ns = "onto-test-rowclass"
	cleanNamespace(t, pool, ns)
	cleanObs := func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM observations WHERE namespace_id = $1`, ns)
	}
	cleanObs()
	t.Cleanup(cleanObs)
	o, err := Load(ctx, pool, seedDoc(), authorities())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO evidence (id, namespace_id, source_id, source_key, content_hash, classification)
		VALUES ('onto-rc-ev',$1,'test','onto-rc-ev','sha256:test','{team:a}')`, ns); err != nil {
		t.Fatal(err)
	}
	const ev = domain.EvidenceID("onto-rc-ev")
	hr := []string{"hr"}
	entities := postgres.NewPgEntityStore(pool, o)
	for _, e := range []string{"order/2", "order/5"} {
		if _, err := entities.EnsureEntity(ctx, domain.Entity{ID: e, NamespaceID: ns, FirstSeenEvidence: ev}); err != nil {
			t.Fatal(err)
		}
	}
	// ADR-0158 stage 5 (D3 site 50): order/7 is minted by a restricted row and
	// order/8 by an unrestricted one; neither holds a link. The required-link
	// rule names an entity only to a reader who may know it exists.
	for _, m := range []struct {
		id   string
		tags []string
	}{{"order/7", hr}, {"order/8", nil}} {
		if _, err := entities.EnsureEntity(ctx, domain.Entity{ID: m.id, NamespaceID: ns, FirstSeenEvidence: ev,
			FirstSeenClassification: m.tags}); err != nil {
			t.Fatal(err)
		}
	}
	links := postgres.NewPgLinkStore(pool, o)
	put := func(from, to string, class []string) {
		t.Helper()
		if _, err := links.PutLink(ctx, domain.Link{NamespaceID: ns, Relation: "placed_by", FromRef: from, ToRef: to,
			State: domain.LinkStateConfirmed, Mechanism: domain.LinkMechanismRecord, Producer: "test@1", EvidenceID: ev,
			AssertedBy: "test", SourceRef: from + ">" + to, Classification: class}); err != nil {
			t.Fatalf("link %s→%s: %v", from, to, err)
		}
	}
	put("entity:invoice/9", "entity:customer/1", hr) // from_kind, restricted
	put("entity:order/2", "entity:customer/1", nil)  // cardinality: one plain ...
	put("entity:order/2", "entity:customer/2", hr)   // ... and one restricted target
	put("entity:order/5", "entity:customer/3", hr)   // order/5's only link is restricted

	// The writes the declarations below would refuse go through a store with no
	// ontology: they stand for rows written before a declaration tightened.
	events := postgres.NewPgEventStore(pool, nil)
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	obs := func(ref, entity, pred string, v domain.StatementValue, at time.Time, class []string) {
		t.Helper()
		if _, err := events.RecordObservation(ctx, domain.Observation{NamespaceID: ns, EntityID: entity, Predicate: pred,
			Value: v, OccurredAt: at, EvidenceID: ev, SourceRef: ref, Classification: class}); err != nil {
			t.Fatalf("observation %s: %v", ref, err)
		}
	}
	num := func(f float64) domain.StatementValue { return domain.StatementValue{Type: "number", Number: f} }
	obs("rc-1", "sensor/1", "temp_c", num(20), t0, nil)                                     // visible to both ...
	obs("rc-2", "sensor/1", "temp_c", num(95), t0.Add(time.Hour), hr)                       // ... a newer restricted value
	obs("rc-3", "sensor/2", "temp_c", num(90), t0, hr)                                      // restricted, out of bounds
	obs("rc-4", "order/1", "total", domain.StatementValue{Type: "text", Text: "x"}, t0, hr) // type mismatch, restricted

	if res, err := o.Apply(ctx, ns, domain.OntologyDoc{
		Kinds: []domain.KindSpec{{Kind: domain.ObservationKind, Predicates: map[string]domain.ValueSpec{
			"temp_c": {Type: "number", Max: fp(60)}}}},
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
		for _, rule := range []string{domain.ViolationFromKind, domain.ViolationCardinality, domain.ViolationRequiredLink,
			domain.ViolationRange, domain.ViolationType} {
			if got := rulesFor(vs, rule); got != nil {
				out[rule] = got
			}
		}
		return out
	}
	granted := report(&domain.TagPredicate{})
	closed := report(&domain.TagPredicate{ForbiddenTags: hr})
	anyOf := report(&domain.TagPredicate{AnyOfClauses: [][]string{{"team:a"}, {"hr", "finance"}}})

	wantGranted := map[string][]string{
		// Link rules report the link id: one each, present only for this reader.
		domain.ViolationFromKind:     granted[domain.ViolationFromKind],
		domain.ViolationCardinality:  granted[domain.ViolationCardinality],
		domain.ViolationRequiredLink: {"order/7", "order/8"},
		domain.ViolationRange:        {"sensor/1", "sensor/2"},
		domain.ViolationType:         {"order/1"},
	}
	if len(granted[domain.ViolationFromKind]) != 1 || len(granted[domain.ViolationCardinality]) != 1 ||
		!reflect.DeepEqual(granted, wantGranted) {
		t.Errorf("granted report = %v, want %v", granted, wantGranted)
	}
	// hr closed: nothing built on a restricted row; sensor/1's current value is the
	// visible one (20, in bounds); order/5 holds no link this reader may read;
	// order/7, minted by a restricted row, does not exist for it, while order/8
	// (R = ∅) reads as before.
	wantClosed := map[string][]string{domain.ViolationRequiredLink: {"order/5", "order/8"}}
	if !reflect.DeepEqual(closed, wantClosed) {
		t.Errorf("hr-closed report = %v, want %v", closed, wantClosed)
	}
	// Row tags never widen: this reader cannot read the evidence ([team:a] fails
	// the clause [hr, finance]), so it is told nothing — although [team:a, hr]
	// would satisfy the clause under a plain union fence.
	if len(anyOf) != 0 {
		t.Errorf("any-of [hr, finance] report = %v, want nothing (a row tag must not widen)", anyOf)
	}
}
