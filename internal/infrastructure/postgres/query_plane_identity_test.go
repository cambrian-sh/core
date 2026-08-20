package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// The closure's GUARDS and its registry-driven verb set (five-planes step 2;
// FIVE-PLANES-BUILD.md). These are claims about the walk, not about SQL, so they
// run against a fake store: identityClosure touches no pool, and a DSN-gated
// test would leave the most important properties in the half of the suite that
// most machines skip.

// fakeLinkStore answers LinksFor from a table and refuses everything else. Every
// unimplemented method panics rather than returning zero: a walk that silently
// started using Candidates would otherwise pass this file untouched.
type fakeLinkStore struct {
	byRef map[string][]domain.Link
	calls int
}

func (f *fakeLinkStore) LinksFor(_ context.Context, _, ref string, opts domain.LinkQuery) ([]domain.Link, error) {
	f.calls++
	var out []domain.Link
	for _, l := range f.byRef[ref] {
		if opts.Family != "" && l.Family != opts.Family {
			continue
		}
		if opts.State != "" && l.State != opts.State {
			continue
		}
		out = append(out, l)
	}
	return out, nil
}

func (f *fakeLinkStore) PutLink(context.Context, domain.Link) (bool, error) {
	panic("closure must not write")
}

func (f *fakeLinkStore) ConfirmLink(context.Context, string, string, string) (domain.Link, error) {
	panic("closure must not confirm")
}

func (f *fakeLinkStore) RetractLink(context.Context, string, string, string, string) error {
	panic("closure must not retract")
}

func (f *fakeLinkStore) Candidates(context.Context, string, int) ([]domain.Link, error) {
	panic("closure must not read candidates")
}

func (f *fakeLinkStore) RetractByProducer(context.Context, string, string, string) (int, error) {
	panic("closure must not revoke")
}

func aliasLink(id, from, to, relation string) domain.Link {
	return domain.Link{
		ID: id, NamespaceID: defaultNamespace, Family: domain.LinkFamilyIdentity,
		FromRef: from, ToRef: to, Relation: relation,
		State: domain.LinkStateConfirmed, Mechanism: domain.LinkMechanismDeclared,
		EvidenceID: domain.EvidenceID("ev-" + id), AssertedBy: "test", Confidence: 1,
	}
}

// chain wires "entity:a0 → entity:a1 → … → entity:aN" under one verb, both
// directions, the way the store's symmetric read would return them.
func chain(verb string, n int) *fakeLinkStore {
	f := &fakeLinkStore{byRef: map[string][]domain.Link{}}
	for i := 0; i < n; i++ {
		from := fmt.Sprintf("entity:k/a%d", i)
		to := fmt.Sprintf("entity:k/a%d", i+1)
		l := aliasLink(fmt.Sprintf("lnk%d", i), from, to, verb)
		f.byRef[from] = append(f.byRef[from], l)
		f.byRef[to] = append(f.byRef[to], l)
	}
	return f
}

func closurePlane(t *testing.T, links domain.LinkStore, specs []domain.RelationSpec) *PgQueryPlane {
	t.Helper()
	reg, err := domain.NewRelationRegistry(specs)
	if err != nil {
		t.Fatalf("relation registry: %v", err)
	}
	return NewPgQueryPlane(nil, nil, nil, nil, links, nil, reg)
}

func TestIdentityClosure_DefaultDepthStops(t *testing.T) {
	p := closurePlane(t, chain(domain.RelationSameAs, 5), nil)
	got, err := p.identityClosure(context.Background(), "k/a0", domain.ClosureDefaultDepth)
	if err != nil {
		t.Fatalf("closure: %v", err)
	}
	if len(got.members) != domain.ClosureDefaultDepth {
		t.Fatalf("default depth admitted %d members, want %d", len(got.members), domain.ClosureDefaultDepth)
	}
	if got.members[0].entityID != "k/a1" || got.members[1].entityID != "k/a2" {
		t.Fatalf("closure walked the wrong way: %+v", got.members)
	}
	// The justification is what makes a closure reviewable rather than a claim.
	if got.members[0].linkID == "" || got.members[0].mechanism == "" || got.members[0].evidence == "" {
		t.Fatalf("member carries no justification: %+v", got.members[0])
	}
}

// The set cap REFUSES rather than truncating: a trimmed closure answers a
// different question from the one asked, and answers it without saying so.
func TestIdentityClosure_SetCapRefusesLoudly(t *testing.T) {
	f := &fakeLinkStore{byRef: map[string][]domain.Link{}}
	root := "entity:k/a0"
	for i := 1; i <= domain.ClosureMaxEntities+2; i++ {
		to := fmt.Sprintf("entity:k/b%d", i)
		l := aliasLink(fmt.Sprintf("lnk%d", i), root, to, domain.RelationSameAs)
		f.byRef[root] = append(f.byRef[root], l)
	}
	p := closurePlane(t, f, nil)
	_, err := p.identityClosure(context.Background(), "k/a0", 1)
	if !errors.Is(err, domain.ErrCannotExpress) {
		t.Fatalf("want a typed refusal past the set cap, got %v", err)
	}
}

// Fan-out under one mechanism FLAGS the entity and excludes its expansion. It
// must not refuse the query: one producer's bad batch would then deny every
// other closure in the deployment.
func TestIdentityClosure_FanOutFlagsAndExcludes(t *testing.T) {
	f := &fakeLinkStore{byRef: map[string][]domain.Link{}}
	root := "entity:k/a0"
	for i := 0; i <= domain.ClosureMaxLinksPerEntityPerMechanism; i++ {
		to := fmt.Sprintf("entity:k/b%d", i)
		f.byRef[root] = append(f.byRef[root], aliasLink(fmt.Sprintf("lnk%d", i), root, to, domain.RelationSameAs))
	}
	p := closurePlane(t, f, nil)
	got, err := p.identityClosure(context.Background(), "k/a0", 1)
	if err != nil {
		t.Fatalf("fan-out must flag, not refuse: %v", err)
	}
	if len(got.members) != 0 {
		t.Fatalf("a flagged entity must contribute no expansion, got %d members", len(got.members))
	}
	if len(got.flagged) != 1 || got.flagged[0] != root {
		t.Fatalf("the offending entity must be REPORTED, got %v", got.flagged)
	}
}

// The verb set comes from the REGISTRY. A verb that is not declared with
// Closure="identity" is not walked, however identity-shaped its name looks —
// the Zero-Hardcode Rule applied to vocabulary.
func TestIdentityClosure_VerbSetIsRegistryDriven(t *testing.T) {
	const verb = "looks_like"
	f := chain(verb, 2)

	// Declared, but with no closure: nothing expands.
	p := closurePlane(t, f, []domain.RelationSpec{
		{Name: verb, Family: domain.LinkFamilyIdentity},
	})
	got, err := p.identityClosure(context.Background(), "k/a0", 2)
	if err != nil {
		t.Fatalf("closure: %v", err)
	}
	if len(got.members) != 0 {
		t.Fatalf("a non-closure verb was walked: %+v", got.members)
	}

	// The SAME data, with the deployment declaring the verb closes over
	// identity: now it expands. Nothing in the kernel changed.
	p = closurePlane(t, f, []domain.RelationSpec{
		{Name: verb, Family: domain.LinkFamilyIdentity, Symmetric: true, Closure: domain.ClosureIdentity},
	})
	got, err = p.identityClosure(context.Background(), "k/a0", 2)
	if err != nil {
		t.Fatalf("closure: %v", err)
	}
	if len(got.members) != 2 {
		t.Fatalf("declared closure verb was not walked: %+v", got.members)
	}
}

// A closure over a store that holds nothing, or a deployment with no links at
// all, is an EMPTY answer and never an error: "this kernel holds no assertions
// about identity" is true, and a 500 is not.
func TestIdentityClosure_AbsentPlaneAnswersEmpty(t *testing.T) {
	p := NewPgQueryPlane(nil, nil, nil, nil, nil, nil, nil)
	got, err := p.identityClosure(context.Background(), "k/a0", 2)
	if err != nil || len(got.members) != 0 {
		t.Fatalf("absent identity plane must answer empty, got %+v / %v", got, err)
	}
}

// whyRef accepts both call shapes without guessing: an already-typed ref is left
// alone, a bare id becomes an entity ref.
func TestWhyRef_PromotesOnlyBareIDs(t *testing.T) {
	cases := map[string]string{
		"customer/C-1042":        "entity:customer/C-1042",
		"entity:customer/C-1042": "entity:customer/C-1042",
		"event:ev-1":             "event:ev-1",
		"decision:d-1":           "decision:d-1",
		"evidence:ev_x":          "evidence:ev_x",
	}
	for in, want := range cases {
		if got := whyRef(in); got != want {
			t.Fatalf("whyRef(%q) = %q, want %q", in, got, want)
		}
	}
}

// The SCOPE RULE's first half, provable without a database: expansion changes
// which SUBJECTS are asked about and nothing else. perSubject runs the shape
// once per subject — the shape that carries the scope predicate — and stamps
// via_ref, so no row can arrive without having passed its own check.
func TestPerSubject_WidensSubjectsAndStampsProvenance(t *testing.T) {
	q := domain.KnowledgeQuery{Kind: domain.QueryPoint, EntityID: "k/a0", Predicate: "p"}
	var asked []string
	rows, err := perSubject([]string{"k/a0", "k/a1"}, q, func(sq domain.KnowledgeQuery) ([]domain.QueryRow, error) {
		asked = append(asked, sq.EntityID)
		return []domain.QueryRow{{"entity_id": sq.EntityID}}, nil
	})
	if err != nil {
		t.Fatalf("perSubject: %v", err)
	}
	if len(asked) != 2 || asked[0] != "k/a0" || asked[1] != "k/a1" {
		t.Fatalf("expansion must re-ask the SHAPE per subject, asked %v", asked)
	}
	if _, stamped := rows[0]["via_ref"]; stamped {
		t.Fatal("the asked-for subject's own rows must not be stamped via_ref")
	}
	if rows[1]["via_ref"] != domain.EntityRef("k/a1") {
		t.Fatalf("an alias row must name the alias it came from, got %v", rows[1]["via_ref"])
	}
}

// ─── The SCOPE RULE, against a real database ─────────────────────────────────
//
// The property the whole plane rests on: a CONFIRMED identity link may add rows
// a caller was already allowed to read, and may NEVER add a row it was not.
// Alias expansion widens the subject; the authz predicate is still evaluated per
// row against that row's own classification and parties.
//
// It needs an engine because the widening and the filtering happen in different
// places — the closure in Go, the classification through evidence — and a fake
// that answered both would be asserting the thing under test.
//
// Scoped to ids nothing else uses and cleaned up row by row. The query plane
// reads namespace "default" by construction, so this cannot hide in a namespace
// of its own the way link_store_test.go does.
const k2ScopeEntityA = "k2scope/a0"
const k2ScopeEntityB = "k2scope/a1"

func newScopePlane(t *testing.T) (*PgQueryPlane, *pgxpool.Pool, context.Context) {
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
		_, _ = pool.Exec(bg, `DELETE FROM links WHERE from_ref LIKE 'entity:k2scope/%' OR to_ref LIKE 'entity:k2scope/%'`)
		_, _ = pool.Exec(bg, `DELETE FROM observations WHERE entity_id LIKE 'k2scope/%'`)
		_, _ = pool.Exec(bg, `DELETE FROM entities WHERE id LIKE 'k2scope/%'`)
		_, _ = pool.Exec(bg, `DELETE FROM evidence WHERE id LIKE 'k2scope-ev-%'`)
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

func seedClassifiedEvidence(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string, tags []string) domain.EvidenceID {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO evidence (id, namespace_id, source_id, source_key, content_hash, classification)
		VALUES ($1,'default','k2scope-source',$1,'sha256:test',$2)
		ON CONFLICT (id) DO NOTHING`, id, tags); err != nil {
		t.Fatalf("seed evidence %s: %v", id, err)
	}
	return domain.EvidenceID(id)
}

func TestPg_AliasClosureNeverWidensAccess(t *testing.T) {
	plane, pool, ctx := newScopePlane(t)
	events := NewPgEventStore(pool, nil)

	openEv := seedClassifiedEvidence(t, ctx, pool, "k2scope-ev-open", []string{"k2-open"})
	secretEv := seedClassifiedEvidence(t, ctx, pool, "k2scope-ev-secret", []string{"k2-secret"})

	now := time.Now().UTC()
	for _, ob := range []domain.Observation{
		{
			NamespaceID: "default", EntityID: k2ScopeEntityA, Predicate: "p",
			Value:      domain.StatementValue{Type: "text", Text: "readable"},
			OccurredAt: now, Confidence: 1, EvidenceID: openEv, SourceRef: "k2scope-a0",
		},
		{
			NamespaceID: "default", EntityID: k2ScopeEntityB, Predicate: "p",
			Value:      domain.StatementValue{Type: "text", Text: "forbidden"},
			OccurredAt: now, Confidence: 1, EvidenceID: secretEv, SourceRef: "k2scope-a1",
		},
	} {
		if _, err := events.RecordObservation(ctx, ob); err != nil {
			t.Fatalf("record observation for %s: %v", ob.EntityID, err)
		}
	}

	// The confirmed equivalence. Its own evidence is the READABLE one, so the
	// stranger can even see the link — and still must not see what it points at.
	links := NewPgLinkStore(pool, nil)
	if _, err := links.PutLink(ctx, domain.Link{
		NamespaceID: "default", Family: domain.LinkFamilyIdentity,
		FromRef: domain.EntityRef(k2ScopeEntityA), ToRef: domain.EntityRef(k2ScopeEntityB),
		Relation: domain.RelationSameAs, State: domain.LinkStateConfirmed,
		Mechanism: domain.LinkMechanismDeclared, EvidenceID: openEv,
		AssertedBy: "test", SourceRef: "k2scope-link",
	}); err != nil {
		t.Fatalf("put link: %v", err)
	}

	stranger := &domain.TagPredicate{RequiredTags: []string{"k2-open"}}
	q := domain.KnowledgeQuery{
		Kind: domain.QueryPoint, EntityID: k2ScopeEntityA,
		Predicate: "p", ExpandAliases: true,
	}
	res, err := plane.Query(ctx, q, stranger)
	if err != nil {
		t.Fatalf("scoped query: %v", err)
	}
	for _, row := range res.Rows {
		if row["value_text"] == "forbidden" {
			t.Fatal("a confirmed identity link handed a stranger a row its own classification denies")
		}
	}
	if len(res.Rows) != 1 {
		t.Fatalf("the readable row should still answer; got %d rows: %v", len(res.Rows), res.Rows)
	}
	// The closure DID widen the subject — otherwise the test above passes
	// vacuously, proving only that nothing was expanded.
	if res.ClosureSize != 1 {
		t.Fatalf("expansion did not happen, so non-widening was never exercised (closure size %d)", res.ClosureSize)
	}
	if !strings.Contains(res.Guarantee, "1 confirmed aliases") {
		t.Fatalf("a widened answer must say so in its guarantee, got %q", res.Guarantee)
	}

	// The same query as an operator reaches both — which is what makes the
	// stranger's answer a SCOPE result rather than an empty store.
	all, err := plane.Query(ctx, q, &domain.TagPredicate{Bypass: true})
	if err != nil {
		t.Fatalf("bypass query: %v", err)
	}
	if len(all.Rows) != 2 {
		t.Fatalf("bypass should reach both the subject and its alias, got %d rows", len(all.Rows))
	}
}

// The `entity` shape's review half: candidates are an operator's to see and
// nobody else's. An agent that reads an unreviewed proposal as an answer has
// done exactly what the review lane exists to prevent.
func TestPg_EntityOp_CandidatesAreOperatorOnly(t *testing.T) {
	plane, pool, ctx := newScopePlane(t)
	openEv := seedClassifiedEvidence(t, ctx, pool, "k2scope-ev-open", []string{"k2-open"})

	links := NewPgLinkStore(pool, nil)
	if _, err := links.PutLink(ctx, domain.Link{
		NamespaceID: "default", Family: domain.LinkFamilyIdentity,
		FromRef: domain.EntityRef(k2ScopeEntityA), ToRef: domain.EntityRef(k2ScopeEntityB),
		Relation: domain.RelationSameAs, State: domain.LinkStateCandidate,
		Mechanism: domain.LinkMechanismScored, EvidenceID: openEv,
		AssertedBy: "test", Producer: "k2scope@1", SourceRef: "k2scope-cand",
	}); err != nil {
		t.Fatalf("put candidate: %v", err)
	}

	q := domain.KnowledgeQuery{Kind: domain.QueryEntity, EntityID: k2ScopeEntityA}
	scoped, err := plane.Query(ctx, q, &domain.TagPredicate{RequiredTags: []string{"k2-open"}})
	if err != nil {
		t.Fatalf("scoped entity op: %v", err)
	}
	for _, row := range scoped.Rows {
		if row["row"] == "candidate" {
			t.Fatal("a scoped caller was shown an unreviewed candidate")
		}
	}
	operator, err := plane.Query(ctx, q, &domain.TagPredicate{Bypass: true})
	if err != nil {
		t.Fatalf("operator entity op: %v", err)
	}
	seen := false
	for _, row := range operator.Rows {
		if row["row"] == "candidate" {
			seen = true
		}
	}
	if !seen {
		t.Fatal("the operator's review inbox is empty, so the test above proved nothing")
	}
}

// ADR-0128 D12: existence is protected content.
//
// A caller who can reach no row of an entity must not be able to tell it apart
// from an id nothing ever minted. Before D12 the entity op answered such a caller
// with the subject row — existence, kind, created-at, evidence id — and listed
// closure membership with the justification redacted, on the reading that
// membership was harmless metadata. The owner ruled the other way: an identity
// closure is itself a disclosure, because it tells an outsider that two systems'
// records belong to one counterparty.
func TestPg_EntityOp_ExistenceIsWithheldFromUnentitledCallers(t *testing.T) {
	plane, pool, ctx := newScopePlane(t)
	secretEv := seedClassifiedEvidence(t, ctx, pool, "k2scope-ev-secret", []string{"k2-secret"})

	entities := NewPgEntityStore(pool)
	if _, err := entities.EnsureEntity(ctx, domain.Entity{
		ID: k2ScopeEntityA, NamespaceID: "default", FirstSeenEvidence: secretEv,
	}); err != nil {
		t.Fatalf("mint entity: %v", err)
	}

	q := domain.KnowledgeQuery{Kind: domain.QueryEntity, EntityID: k2ScopeEntityA}

	stranger, err := plane.Query(ctx, q, &domain.TagPredicate{RequiredTags: []string{"k2-open"}})
	if err != nil {
		t.Fatalf("stranger entity op: %v", err)
	}
	if len(stranger.Rows) != 0 {
		t.Fatalf("a caller entitled to nothing about this entity learned it exists: %v", stranger.Rows)
	}

	// The same query by someone who may read the minting evidence must answer,
	// or the assertion above is satisfied by an empty store rather than by the
	// rule under test.
	entitled, err := plane.Query(ctx, q, &domain.TagPredicate{RequiredTags: []string{"k2-secret"}})
	if err != nil {
		t.Fatalf("entitled entity op: %v", err)
	}
	if len(entitled.Rows) == 0 || entitled.Rows[0]["row"] != "entity" {
		t.Fatalf("a caller who may read the minting evidence must still get the handle: %v", entitled.Rows)
	}
	if entitled.Rows[0]["minted"] != true {
		t.Fatalf("the subject row should report the entity as minted: %v", entitled.Rows[0])
	}
}

// The other half of the gate: existence is disclosed by ANY row the caller can
// reach, not only by the minting evidence.
//
// An entity is routinely minted from a record a reader cannot see and then cited
// by records they can. Withholding the handle from them would over-refuse in the
// name of a rule about under-refusing, so a reachable link earns the subject row.
func TestPg_EntityOp_AReachableRowStillDisclosesTheHandle(t *testing.T) {
	plane, pool, ctx := newScopePlane(t)
	secretEv := seedClassifiedEvidence(t, ctx, pool, "k2scope-ev-secret", []string{"k2-secret"})
	openEv := seedClassifiedEvidence(t, ctx, pool, "k2scope-ev-open", []string{"k2-open"})

	entities := NewPgEntityStore(pool)
	if _, err := entities.EnsureEntity(ctx, domain.Entity{
		ID: k2ScopeEntityA, NamespaceID: "default", FirstSeenEvidence: secretEv,
	}); err != nil {
		t.Fatalf("mint entity: %v", err)
	}

	// A confirmed link the stranger CAN read, touching the same handle.
	links := NewPgLinkStore(pool, nil)
	if _, err := links.PutLink(ctx, domain.Link{
		NamespaceID: "default", Family: domain.LinkFamilyIdentity,
		FromRef: domain.EntityRef(k2ScopeEntityA), ToRef: domain.EntityRef(k2ScopeEntityB),
		Relation: domain.RelationSameAs, State: domain.LinkStateConfirmed,
		Mechanism: domain.LinkMechanismDeclared, EvidenceID: openEv,
		AssertedBy: "test", SourceRef: "k2scope-reachable",
	}); err != nil {
		t.Fatalf("put link: %v", err)
	}

	q := domain.KnowledgeQuery{Kind: domain.QueryEntity, EntityID: k2ScopeEntityA}
	res, err := plane.Query(ctx, q, &domain.TagPredicate{RequiredTags: []string{"k2-open"}})
	if err != nil {
		t.Fatalf("stranger entity op: %v", err)
	}
	if len(res.Rows) == 0 {
		t.Fatal("a caller who can read a link on this entity must still get an answer")
	}
	if res.Rows[0]["row"] != "entity" {
		t.Fatalf("the subject row should lead the answer: %v", res.Rows)
	}
}
