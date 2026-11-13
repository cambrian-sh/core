package ontology

import (
	"context"
	"errors"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
)

// Integration tests over real Postgres (PG_TEST_DSN; migration 0040). The
// declarations table is global, so each test starts from an empty one
// (TRUNCATE — a test database only) and keeps its rows in its own namespace.

func pgPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping integration test that requires PostgreSQL")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE ontology_declarations`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool, ctx
}

func cleanNamespace(t *testing.T, pool *pgxpool.Pool, ns string) {
	t.Helper()
	clean := func() {
		bg := context.Background()
		for _, q := range []string{
			`DELETE FROM resolutions WHERE namespace_id = $1`,
			`DELETE FROM knowledge_items WHERE namespace_id = $1`,
			`DELETE FROM links WHERE namespace_id = $1`,
			`DELETE FROM entities WHERE namespace_id = $1`,
			`DELETE FROM evidence WHERE namespace_id = $1`,
		} {
			_, _ = pool.Exec(bg, q, ns)
		}
	}
	clean()
	t.Cleanup(clean)
}

func countRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ontology_declarations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPg_LoadSeedsOnceAndReloadsAppliedChanges(t *testing.T) {
	pool, ctx := pgPool(t)
	o, err := Load(ctx, pool, seedDoc(), authorities())
	if err != nil {
		t.Fatal(err)
	}
	if v := o.Current("").Version; v != 1 || countRows(t, ctx, pool) != 1 {
		t.Fatalf("a fresh table seeds version 1, got %d (%d rows)", v, countRows(t, ctx, pool))
	}
	// Same seed: nothing new.
	if o, err = Load(ctx, pool, seedDoc(), authorities()); err != nil || o.Current("x").Version != 1 || countRows(t, ctx, pool) != 1 {
		t.Fatalf("re-boot with the same seed wrote a row (err %v)", err)
	}
	// The same seed in another order is the same seed.
	reordered := seedDoc()
	reordered.Kinds[0], reordered.Kinds[1] = reordered.Kinds[1], reordered.Kinds[0]
	if _, err = Load(ctx, pool, reordered, authorities()); err != nil || countRows(t, ctx, pool) != 1 {
		t.Fatalf("spec order changed the seed (err %v)", err)
	}
	// Apply a change in one namespace.
	res, err := o.Apply(ctx, "tenant-a", domain.OntologyDoc{Kinds: []domain.KindSpec{{Kind: "invoice",
		Predicates: map[string]domain.ValueSpec{"amount": {Type: "number"}}}}}, "user:op", false)
	if err != nil || !res.Applied || res.Version != 2 {
		t.Fatalf("apply: %+v %v", res, err)
	}
	var author, ns string
	if err := pool.QueryRow(ctx, `SELECT author, namespace FROM ontology_declarations WHERE version = 2`).Scan(&author, &ns); err != nil ||
		author != "user:op" || ns != "tenant-a" {
		t.Fatalf("row: %q %q %v", author, ns, err)
	}
	// A changed seed (code gained a kind) appends a base row, and the applied
	// change survives the restart on top of it.
	seed2 := seedDoc()
	seed2.Kinds = append(seed2.Kinds, domain.KindSpec{Kind: "shipment", Predicates: map[string]domain.ValueSpec{"eta": {Type: "date"}}})
	o2, err := Load(ctx, pool, seed2, authorities())
	if err != nil {
		t.Fatal(err)
	}
	if countRows(t, ctx, pool) != 3 {
		t.Fatalf("rows = %d, want 3", countRows(t, ctx, pool))
	}
	a := o2.Current("tenant-a")
	if a.Version != 3 {
		t.Fatalf("tenant-a version = %d, want 3 (the new base)", a.Version)
	}
	if _, ok := a.Kinds.Spec("invoice"); !ok {
		t.Fatal("the applied change did not survive the restart")
	}
	if _, ok := a.Kinds.Spec("shipment"); !ok {
		t.Fatal("the new boot declaration is missing in tenant-a")
	}
	if _, ok := o2.Current("default").Kinds.Spec("invoice"); ok {
		t.Fatal("a namespace change leaked into another namespace")
	}
	// Append-only.
	if _, err := pool.Exec(ctx, `UPDATE ontology_declarations SET author = 'x'`); err == nil {
		t.Fatal("ontology_declarations must refuse UPDATE")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM ontology_declarations`); err == nil {
		t.Fatal("ontology_declarations must refuse DELETE")
	}
}

// Characterization (ADR-0149 D2 "nothing changes for existing deployments"):
// the registries a boot built directly from its sources before this change, and
// the snapshot the live ontology serves after it, answer identically — on the
// first boot and after a reload from the table.
func TestPg_BootSnapshotEqualsTodaysRegistries(t *testing.T) {
	pool, ctx := pgPool(t)
	// The kernel's sources: plugin kinds (as the substrate plugin and the ingress
	// studio declare them), core's memory_fact, plugin + config relations.
	kinds := []domain.KindSpec{
		{Kind: "policy_statement", Predicates: map[string]domain.ValueSpec{"bound_kind": {Type: "text"}, "refund_window_days": {Type: "number"}}},
		{Kind: "decision", Policy: domain.ResolutionPolicyDesignatedActor, ResolutionScope: domain.ResolutionScopeEntity,
			Designated: &domain.DesignatedActors{Principals: []string{"user:boss"}, ApprovedViaCommit: true},
			Predicates: map[string]domain.ValueSpec{"state": {Type: "text", Enum: []string{"open", "closed"}}, "amount": {Type: "number", Min: fp(0)}},
			Lifecycle:  &domain.KindLifecycle{StatePredicate: "state", Initial: []string{"open"}, Transitions: map[string][]string{"open": {"closed"}}},
			EventTypes: []domain.EventTypeSpec{{Type: "decision_opened", Roles: []string{"by"}}}},
		domain.MemoryFactKindSpec(),
	}
	rels := []domain.RelationSpec{
		{Name: "mapped_from", Family: domain.LinkFamilyLineage},
		{Name: "duplicate_of", Family: domain.LinkFamilyIdentity, Symmetric: true, Closure: domain.ClosureIdentity},
		{Name: "subsidiary_of", Family: domain.LinkFamilyRelation, Closure: domain.ClosureRollup},
	}
	auths := authorities()
	oldKinds, err := domain.NewKindRegistry(kinds, auths)
	if err != nil {
		t.Fatal(err)
	}
	oldRels, err := domain.NewRelationRegistry(rels)
	if err != nil {
		t.Fatal(err)
	}
	for boot := 1; boot <= 2; boot++ {
		o, err := Load(ctx, pool, domain.OntologyDoc{Kinds: kinds, Relations: rels}, auths)
		if err != nil {
			t.Fatal(err)
		}
		snap := o.Current("default")
		for _, k := range []string{"policy_statement", "decision", domain.KindMemoryFact, "undeclared"} {
			a, aok := oldKinds.Spec(k)
			b, bok := snap.Kinds.Spec(k)
			if aok != bok || !reflect.DeepEqual(a, b) {
				t.Fatalf("boot %d kind %s differs:\n%+v\n%+v", boot, k, a, b)
			}
			if oldKinds.EntityScoped(k) != snap.Kinds.EntityScoped(k) || oldKinds.HasLifecycle(k) != snap.Kinds.HasLifecycle(k) {
				t.Fatalf("boot %d kind %s: derived answers differ", boot, k)
			}
		}
		for _, p := range []string{domain.ResolutionPolicyLatestAssertion, domain.ResolutionPolicyDesignatedActor,
			domain.PolicyValidTimeSupersession, "nope"} {
			_, aok := oldKinds.Authority(p)
			_, bok := snap.Kinds.Authority(p)
			if aok != bok {
				t.Fatalf("authority %s differs", p)
			}
		}
		if !reflect.DeepEqual(oldRels.Verbs(), snap.Relations.Verbs()) ||
			!reflect.DeepEqual(oldRels.SymmetricVerbs(), snap.Relations.SymmetricVerbs()) ||
			!reflect.DeepEqual(oldRels.ClosureVerbs(domain.ClosureIdentity), snap.Relations.ClosureVerbs(domain.ClosureIdentity)) ||
			!reflect.DeepEqual(oldRels.ClosureVerbs(domain.ClosureRollup), snap.Relations.ClosureVerbs(domain.ClosureRollup)) {
			t.Fatalf("boot %d relation sets differ", boot)
		}
		for _, v := range oldRels.Verbs() {
			a, _ := oldRels.Spec(v)
			b, _ := snap.Relations.Spec(v)
			if !reflect.DeepEqual(a, b) {
				t.Fatalf("verb %s differs", v)
			}
		}
	}
	if countRows(t, ctx, pool) != 1 {
		t.Fatalf("two boots with one seed wrote %d rows", countRows(t, ctx, pool))
	}
}

func fp(v float64) *float64 { return &v }

func orderItem(ns, entity, status string, tags []string, extra ...domain.StatementValue) domain.KnowledgeItem {
	vals := []domain.StatementValue{{Predicate: "status", Type: "text", Text: status}}
	return domain.KnowledgeItem{NamespaceID: ns, Kind: "order", EntityID: entity, AssertedBy: "user:clerk",
		AssertedAt: time.Now().UTC(), SourceRef: entity + "|" + status, Classification: tags,
		Values: append(vals, extra...)}
}

func rulesFor(vs []domain.OntologyViolation, rule string) []string {
	var out []string
	for _, v := range vs {
		if v.Rule == rule {
			out = append(out, v.Ref)
		}
	}
	sort.Strings(out)
	return out
}

func TestPg_LiveChangesReachTheNextWrite_TighteningIsReportedNotRetroRejected(t *testing.T) {
	pool, ctx := pgPool(t)
	const ns = "onto-test-items"
	cleanNamespace(t, pool, ns)
	o, err := Load(ctx, pool, seedDoc(), authorities())
	if err != nil {
		t.Fatal(err)
	}
	store := postgres.NewPgKnowledgeStore(pool, o)
	all := &domain.TagPredicate{Bypass: true}

	// Undeclared predicate: refused.
	eta := domain.StatementValue{Predicate: "eta", Type: "date", Date: time.Now().UTC()}
	if _, _, err := store.PutItem(ctx, orderItem(ns, "order/1", "open", nil, eta)); !errors.Is(err, domain.ErrKindRefused) {
		t.Fatalf("undeclared predicate: %v", err)
	}
	// Additive change, live: the very next write passes, no restart.
	add := domain.OntologyDoc{Kinds: []domain.KindSpec{{Kind: "order", Predicates: map[string]domain.ValueSpec{
		"status": {Type: "text", Enum: []string{"open", "shipped", "lost"}}, "total": {Type: "number"}, "eta": {Type: "date"}}}}}
	res, err := o.Apply(ctx, ns, add, "user:op", false)
	if err != nil || res.Class != domain.ChangeAdditive || !res.Applied {
		t.Fatalf("additive: %+v %v", res, err)
	}
	id1, _, err := store.PutItem(ctx, orderItem(ns, "order/1", "open", []string{"team:a"}, eta))
	if err != nil {
		t.Fatalf("write after the additive change: %v", err)
	}
	var dv *int64
	if err := pool.QueryRow(ctx, `SELECT decl_version FROM knowledge_items WHERE id = $1`, string(id1)).Scan(&dv); err != nil ||
		dv == nil || *dv != res.Version {
		t.Fatalf("decl_version = %v, want %d (%v)", dv, res.Version, err)
	}
	lost, _, err := store.PutItem(ctx, orderItem(ns, "order/2", "lost", []string{"team:a"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.PutItem(ctx, orderItem(ns, "order/3", "lost", []string{"team:b"}, domain.StatementValue{Predicate: "total", Type: "number", Number: -5})); err != nil {
		t.Fatal(err)
	}

	// Tightening: narrow the enum, require total, bound total.
	tight := domain.OntologyDoc{Kinds: []domain.KindSpec{{Kind: "order", Predicates: map[string]domain.ValueSpec{
		"status": {Type: "text", Enum: []string{"open", "shipped"}}, "total": {Type: "number", Required: true, Min: fp(0)},
		"eta": {Type: "date"}}}}}
	res, err = o.Apply(ctx, ns, tight, "user:op", false)
	if err != nil || res.Class != domain.ChangeTightening || !res.Applied {
		t.Fatalf("tightening: %+v %v", res, err)
	}
	// New bad writes are refused...
	if _, _, err := store.PutItem(ctx, orderItem(ns, "order/4", "lost", nil, domain.StatementValue{Predicate: "total", Type: "number", Number: 1})); !errors.Is(err, domain.ErrKindRefused) {
		t.Fatalf("a narrowed enum must refuse a new write: %v", err)
	}
	if _, _, err := store.PutItem(ctx, orderItem(ns, "order/4", "open", nil)); !errors.Is(err, domain.ErrKindRefused) {
		t.Fatalf("a required predicate must refuse a new write without it: %v", err)
	}
	// ...the old rows stay, and are reported.
	vs, truncated, err := o.Violations(ctx, all, ns, 0)
	if err != nil || truncated {
		t.Fatalf("violations: %v (truncated %v)", err, truncated)
	}
	if got := rulesFor(vs, domain.ViolationEnum); len(got) != 2 || (got[0] != string(lost) && got[1] != string(lost)) {
		t.Fatalf("enum violations = %v (%+v)", got, vs)
	}
	if got := rulesFor(vs, domain.ViolationRequired); len(got) != 2 {
		t.Fatalf("required violations = %v", got)
	}
	if got := rulesFor(vs, domain.ViolationRange); len(got) != 1 {
		t.Fatalf("range violations = %v", got)
	}
	// Fenced: a reader of team:b sees only team:b's row.
	narrow, _, err := o.Violations(ctx, &domain.TagPredicate{RequiredTags: []string{"team:b"}}, ns, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range narrow {
		if v.Ref == string(lost) || v.Ref == string(id1) {
			t.Fatalf("a team:a row leaked to a team:b reader: %+v", v)
		}
	}
	// order/3 (team:b): its status is outside the enum and its total below the
	// minimum; it carries total, so no required violation.
	if len(narrow) != 2 || len(rulesFor(narrow, domain.ViolationEnum)) != 1 || len(rulesFor(narrow, domain.ViolationRange)) != 1 {
		t.Fatalf("narrow report = %+v", narrow)
	}
	// Limit truncates.
	if vs, truncated, err := o.Violations(ctx, all, ns, 1); err != nil || !truncated || len(vs) != 1 {
		t.Fatalf("limit 1: %d %v %v", len(vs), truncated, err)
	}
	// Breaking: refused, nothing written, version unchanged.
	before := countRows(t, ctx, pool)
	_, err = o.Apply(ctx, ns, domain.OntologyDoc{Kinds: []domain.KindSpec{{Kind: "order", Predicates: map[string]domain.ValueSpec{
		"status": {Type: "text"}}}}}, "user:op", false)
	if !errors.Is(err, domain.ErrOntologyChangeRefused) || countRows(t, ctx, pool) != before || o.Current(ns).Version != res.Version {
		t.Fatalf("breaking change: %v", err)
	}
}

func TestPg_TypedLinks_RefuseNewViolationsAndReportOldOnes(t *testing.T) {
	pool, ctx := pgPool(t)
	const ns = "onto-test-links"
	cleanNamespace(t, pool, ns)
	o, err := Load(ctx, pool, seedDoc(), authorities())
	if err != nil {
		t.Fatal(err)
	}
	links := postgres.NewPgLinkStore(pool, o)
	entities := postgres.NewPgEntityStore(pool, o)
	evidence := func(id string, tags []string) domain.EvidenceID {
		if _, err := pool.Exec(ctx, `INSERT INTO evidence (id, namespace_id, source_id, source_key, content_hash, classification)
			VALUES ($1,$2,'test',$1,'sha256:test',$3)`, id, ns, tags); err != nil {
			t.Fatal(err)
		}
		return domain.EvidenceID(id)
	}
	evA, evB := evidence("onto-ev-a", []string{"team:a"}), evidence("onto-ev-b", []string{"team:b"})
	for _, e := range []string{"order/1", "order/2", "order/3", "order/4"} {
		if _, err := entities.EnsureEntity(ctx, domain.Entity{ID: e, NamespaceID: ns, FirstSeenEvidence: evA}); err != nil {
			t.Fatal(err)
		}
	}
	put := func(from, to, state, mech string, ev domain.EvidenceID, validFrom, validTo *time.Time) (bool, error) {
		return links.PutLink(ctx, domain.Link{NamespaceID: ns, Relation: "placed_by", FromRef: from, ToRef: to,
			State: state, Mechanism: mech, Producer: "test@1", EvidenceID: ev, AssertedBy: "test",
			SourceRef: from + ">" + to + state, ValidFrom: validFrom, ValidTo: validTo})
	}
	conf := domain.LinkStateConfirmed
	// Before the rules: all admitted.
	for _, l := range [][2]string{{"entity:order/1", "entity:customer/1"}, {"entity:order/1", "entity:customer/2"}, {"entity:invoice/9", "entity:customer/1"}} {
		if _, err := put(l[0], l[1], conf, domain.LinkMechanismRecord, evA, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := put("entity:order/3", "event:e1", conf, domain.LinkMechanismRecord, evB, nil, nil); err != nil {
		t.Fatal(err)
	}

	res, err := o.Apply(ctx, ns, domain.OntologyDoc{Relations: []domain.RelationSpec{{Name: "placed_by", Family: domain.LinkFamilyRelation,
		FromKind: "order", ToKind: "customer", Cardinality: domain.CardinalityOne, Required: true, Inverse: "placed"}}}, "user:op", false)
	if err != nil || res.Class != domain.ChangeTightening {
		t.Fatalf("typed tightening: %+v %v", res, err)
	}

	// New confirmed violations are refused with a precise error.
	if _, err := put("entity:invoice/8", "entity:customer/1", conf, domain.LinkMechanismRecord, evA, nil, nil); !errors.Is(err, domain.ErrLinkRefused) ||
		!strings.Contains(err.Error(), `from endpoint to be a "order" entity`) {
		t.Fatalf("wrong from kind: %v", err)
	}
	// A candidate is a proposal: not checked.
	if _, err := put("entity:invoice/8", "entity:customer/1", domain.LinkStateCandidate, domain.LinkMechanismScored, evA, nil, nil); err != nil {
		t.Fatalf("candidate: %v", err)
	}
	// Confirming it is a new confirmed link: refused.
	cands, err := links.Candidates(ctx, ns, 10)
	if err != nil || len(cands) != 1 {
		t.Fatalf("candidates: %v %v", cands, err)
	}
	if _, err := links.ConfirmLink(ctx, ns, cands[0].ID, "user:reviewer"); !errors.Is(err, domain.ErrLinkRefused) {
		t.Fatalf("confirming a wrongly typed candidate: %v", err)
	}
	// Cardinality one: a second overlapping target is refused...
	if created, err := put("entity:order/2", "entity:customer/1", conf, domain.LinkMechanismRecord, evA, nil, nil); err != nil || !created {
		t.Fatalf("first target: %v", err)
	}
	if _, err := put("entity:order/2", "entity:customer/2", conf, domain.LinkMechanismRecord, evA, nil, nil); !errors.Is(err, domain.ErrLinkRefused) ||
		!strings.Contains(err.Error(), "cardinality one") {
		t.Fatalf("second overlapping target: %v", err)
	}
	// ...a replay of the same target is not a second target...
	if created, err := put("entity:order/2", "entity:customer/1", conf, domain.LinkMechanismRecord, evA, nil, nil); err != nil || created {
		t.Fatalf("replay: created=%v %v", created, err)
	}
	// ...and a reassignment with disjoint validity windows is allowed.
	t0 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.AddDate(0, 1, 0)
	if _, err := put("entity:order/4", "entity:customer/1", conf, domain.LinkMechanismRecord, evA, nil, &t0); err != nil {
		t.Fatalf("bounded first owner: %v", err)
	}
	if _, err := put("entity:order/4", "entity:customer/2", conf, domain.LinkMechanismRecord, evA, &t1, nil); err != nil {
		t.Fatalf("disjoint second owner: %v", err)
	}
	if _, err := put("entity:order/4", "entity:customer/3", conf, domain.LinkMechanismRecord, evA, &t0, &t1); err != nil {
		t.Fatalf("a window between the two touches neither: %v", err)
	}
	if _, err := put("entity:order/4", "entity:customer/4", conf, domain.LinkMechanismRecord, evA, &t0, nil); !errors.Is(err, domain.ErrLinkRefused) {
		t.Fatalf("a window overlapping the second owner must refuse: %v", err)
	}
	var dv *int64
	if err := pool.QueryRow(ctx, `SELECT decl_version FROM links WHERE namespace_id=$1 AND to_ref='entity:customer/2' AND from_ref='entity:order/4'`, ns).Scan(&dv); err != nil ||
		dv == nil || *dv != res.Version {
		t.Fatalf("link decl_version = %v (%v), want %d", dv, err, res.Version)
	}

	// The report lists what was written before the rules.
	vs, _, err := o.Violations(ctx, &domain.TagPredicate{Bypass: true}, ns, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := rulesFor(vs, domain.ViolationFromKind); len(got) != 1 {
		t.Fatalf("from_kind = %v (%+v)", got, vs)
	}
	if got := rulesFor(vs, domain.ViolationToKind); len(got) != 1 {
		t.Fatalf("to_kind = %v (%+v)", got, vs)
	}
	if got := rulesFor(vs, domain.ViolationCardinality); len(got) != 1 {
		t.Fatalf("cardinality = %v (%+v)", got, vs)
	}
	// order/1..4 all hold a link now; mint one that does not.
	if _, err := entities.EnsureEntity(ctx, domain.Entity{ID: "order/9", NamespaceID: ns, FirstSeenEvidence: evA}); err != nil {
		t.Fatal(err)
	}
	vs, _, _ = o.Violations(ctx, &domain.TagPredicate{Bypass: true}, ns, 0)
	if got := rulesFor(vs, domain.ViolationRequiredLink); len(got) != 1 || got[0] != "order/9" {
		t.Fatalf("required_link = %v (%+v)", got, vs)
	}
	// Fenced: a team:b reader cannot read evA, so sees no link and no entity built on it.
	narrow, _, err := o.Violations(ctx, &domain.TagPredicate{RequiredTags: []string{"team:b"}}, ns, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range narrow {
		if v.Rule != domain.ViolationToKind {
			t.Fatalf("a team:b reader saw %+v", v)
		}
	}
	if len(narrow) != 1 {
		t.Fatalf("team:b reader should see exactly its own to_kind violation, got %+v", narrow)
	}
}

// Two writers racing to give one order two customers: exactly one wins.
func TestPg_CardinalityOneUnderConcurrency(t *testing.T) {
	pool, ctx := pgPool(t)
	const ns = "onto-test-race"
	cleanNamespace(t, pool, ns)
	seed := seedDoc()
	seed.Relations = []domain.RelationSpec{{Name: "placed_by", Family: domain.LinkFamilyRelation, Cardinality: domain.CardinalityOne}}
	o, err := Load(ctx, pool, seed, authorities())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO evidence (id, namespace_id, source_id, source_key, content_hash)
		VALUES ('onto-race-ev',$1,'t','k','sha256:t')`, ns); err != nil {
		t.Fatal(err)
	}
	links := postgres.NewPgLinkStore(pool, o)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = links.PutLink(ctx, domain.Link{NamespaceID: ns, Relation: "placed_by", FromRef: "entity:order/1",
				ToRef: "entity:customer/" + string(rune('a'+i)), State: domain.LinkStateConfirmed, Mechanism: domain.LinkMechanismRecord,
				EvidenceID: "onto-race-ev", AssertedBy: "t", SourceRef: string(rune('a' + i))})
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		} else if !errors.Is(err, domain.ErrLinkRefused) {
			t.Fatalf("unexpected: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d writers won, want exactly 1", ok)
	}
}
