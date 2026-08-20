package postgres

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// The identity plane's store-level guarantees, against a real database.
//
// They are claims about what POSTGRES does — that links_dedup collapses a replay, that a
// retraction touches two columns and no others, that a confirmation appends rather than
// overwrites — and a test asserting the SQL string contained "ON CONFLICT" would pass
// just as happily against a statement aimed at the wrong table. The pure semantics
// (refusals, canonical ordering) are proved without a DB in domain/identity_test.go; this
// file covers only what needs the engine.

const identityNS = "test-identity"

func newIdentityStores(t *testing.T) (*PgEntityStore, *PgLinkStore, *pgxpool.Pool, context.Context) {
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

	// Scoped to its own namespace and cleaned up, so this cannot disturb — or be
	// disturbed by — anything else in a shared test database. Links go first: they
	// carry the FK into evidence.
	clean := func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM links WHERE namespace_id = $1`, identityNS)
		_, _ = pool.Exec(bg, `DELETE FROM entities WHERE namespace_id = $1`, identityNS)
		_, _ = pool.Exec(bg, `DELETE FROM evidence WHERE namespace_id = $1`, identityNS)
	}
	clean()
	t.Cleanup(clean)

	reg, err := domain.NewRelationRegistry([]domain.RelationSpec{
		{Name: "subsidiary_of", Family: domain.LinkFamilyRelation, Closure: domain.ClosureRollup},
	})
	if err != nil {
		t.Fatalf("relation registry: %v", err)
	}
	return NewPgEntityStore(pool), NewPgLinkStore(pool, reg), pool, ctx
}

// seedEvidence inserts the minimum evidence row a link's FK will accept. Links REFERENCE
// evidence(id), which is the admissibility rule expressed as a constraint: a machine
// assertion cannot point at a basis that does not exist.
func seedEvidence(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string) domain.EvidenceID {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO evidence (id, namespace_id, source_id, source_key, content_hash)
		VALUES ($1,$2,'test-source',$1,'sha256:test')
		ON CONFLICT (id) DO NOTHING`, id, identityNS); err != nil {
		t.Fatalf("seed evidence: %v", err)
	}
	return domain.EvidenceID(id)
}

func candidate(ev domain.EvidenceID) domain.Link {
	return domain.Link{
		NamespaceID: identityNS,
		FromRef:     domain.EntityRef("customer/C-1042"),
		ToRef:       domain.EntityRef("customer/ACME-9"),
		Relation:    domain.RelationSameAs,
		Mechanism:   domain.LinkMechanismScored,
		Producer:    "id_shape@v1",
		Confidence:  0.71,
		EvidenceID:  ev,
		AssertedBy:  "producer:id_shape",
		SourceRef:   "orders/row-1",
	}
}

func TestPgLinkStore_PutLinkIsIdempotent(t *testing.T) {
	_, links, pool, ctx := newIdentityStores(t)
	ev := seedEvidence(t, ctx, pool, "ev-idem")

	created, err := links.PutLink(ctx, candidate(ev))
	if err != nil {
		t.Fatalf("first put: %v", err)
	}
	if !created {
		t.Fatalf("first put reported created=false")
	}
	// The same producer replaying the same delivery must write NOTHING — this is
	// what makes reprocessing an outbox safe rather than a graph-doubling event.
	created, err = links.PutLink(ctx, candidate(ev))
	if err != nil {
		t.Fatalf("replay put: %v", err)
	}
	if created {
		t.Fatalf("replay reported created=true — links_dedup did not collapse it")
	}

	// A DIFFERENT mechanism asserting the same thing is a SECOND row, on purpose:
	// corroboration is information, and read paths deduplicate.
	corroborating := candidate(ev)
	corroborating.Mechanism = domain.LinkMechanismDeclared
	corroborating.Producer = "mapping/orders@v3"
	corroborating.State = domain.LinkStateConfirmed
	created, err = links.PutLink(ctx, corroborating)
	if err != nil {
		t.Fatalf("corroborating put: %v", err)
	}
	if !created {
		t.Fatalf("a second mechanism was collapsed into the first row")
	}
}

func TestPgLinkStore_CanonicalOrderingCollapsesBothDirections(t *testing.T) {
	_, links, pool, ctx := newIdentityStores(t)
	ev := seedEvidence(t, ctx, pool, "ev-canon")

	l := candidate(ev)
	if _, err := links.PutLink(ctx, l); err != nil {
		t.Fatalf("put: %v", err)
	}
	// The SAME equivalence, asserted the other way round. Without canonical
	// ordering this is a second row that no dedup key could ever reconcile and a
	// reviewer would be asked to confirm twice.
	reversed := l
	reversed.FromRef, reversed.ToRef = l.ToRef, l.FromRef
	created, err := links.PutLink(ctx, reversed)
	if err != nil {
		t.Fatalf("reversed put: %v", err)
	}
	if created {
		t.Fatalf("A→B and B→A were stored as two rows")
	}

	got, err := links.LinksFor(ctx, identityNS, l.FromRef, domain.LinkQuery{})
	if err != nil {
		t.Fatalf("LinksFor: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("LinksFor returned %d rows, want 1", len(got))
	}
	if got[0].FromRef >= got[0].ToRef {
		t.Fatalf("stored endpoints are not ordered: %s → %s", got[0].FromRef, got[0].ToRef)
	}
	// Symmetric verbs answer for EITHER endpoint: canonical ordering decided which
	// column the ref landed in, and that is not a statement about direction.
	other, err := links.LinksFor(ctx, identityNS, l.ToRef, domain.LinkQuery{})
	if err != nil {
		t.Fatalf("LinksFor(other endpoint): %v", err)
	}
	if len(other) != 1 {
		t.Fatalf("the symmetric verb did not answer for its other endpoint (%d rows)", len(other))
	}
}

func TestPgLinkStore_StoreEnforcesTheRefusals(t *testing.T) {
	_, links, pool, ctx := newIdentityStores(t)
	ev := seedEvidence(t, ctx, pool, "ev-refuse")

	// The rules are pure functions, but the STORE is the chokepoint: a producer
	// that skipped them must still be refused here, or the rules bind only the
	// producers that remembered them.
	ceiling := candidate(ev)
	ceiling.State = domain.LinkStateConfirmed
	if _, err := links.PutLink(ctx, ceiling); !errors.Is(err, domain.ErrLinkRefused) {
		t.Fatalf("a scored mechanism wrote confirmed: err=%v", err)
	}

	noBasis := candidate(ev)
	noBasis.EvidenceID = ""
	if _, err := links.PutLink(ctx, noBasis); !errors.Is(err, domain.ErrLinkRefused) {
		t.Fatalf("a machine assertion with no evidence was admitted: err=%v", err)
	}

	unknownVerb := candidate(ev)
	unknownVerb.Relation = "looks_like"
	if _, err := links.PutLink(ctx, unknownVerb); !errors.Is(err, domain.ErrLinkRefused) {
		t.Fatalf("an undeclared verb was admitted: err=%v", err)
	}

	// Nothing landed.
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM links WHERE namespace_id=$1`, identityNS).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d refused links reached the table", n)
	}
}

func TestPgLinkStore_ConfirmAppendsAndLeavesTheCandidateAlone(t *testing.T) {
	_, links, pool, ctx := newIdentityStores(t)
	ev := seedEvidence(t, ctx, pool, "ev-confirm")

	if _, err := links.PutLink(ctx, candidate(ev)); err != nil {
		t.Fatalf("put: %v", err)
	}
	pending, err := links.Candidates(ctx, identityNS, 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("Candidates: %v (%d rows)", err, len(pending))
	}
	original := pending[0]

	confirmed, err := links.ConfirmLink(ctx, identityNS, original.ID, "operator:ada")
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if confirmed.ID == original.ID {
		t.Fatalf("confirmation rewrote the candidate instead of appending")
	}
	if confirmed.State != domain.LinkStateConfirmed || confirmed.Mechanism != domain.LinkMechanismHuman {
		t.Fatalf("confirmation row has the wrong state/mechanism: %+v", confirmed)
	}
	if confirmed.AssertedBy != "operator:ada" {
		t.Fatalf("confirmation does not name its actor: %q", confirmed.AssertedBy)
	}
	// The basis is INHERITED: the human endorses the candidate's evidence rather
	// than inventing a new one, which is what stops confirmation from being the
	// admissibility rule defeated by a click.
	if confirmed.EvidenceID != original.EvidenceID {
		t.Fatalf("confirmation did not inherit the evidence: %q vs %q", confirmed.EvidenceID, original.EvidenceID)
	}

	// The proposal is untouched — the review lane's product is the record of what
	// the producer proposed AND what the person made of it.
	still, err := links.LinksFor(ctx, identityNS, original.FromRef, domain.LinkQuery{State: domain.LinkStateCandidate})
	if err != nil {
		t.Fatalf("LinksFor: %v", err)
	}
	if len(still) != 1 || still[0].ID != original.ID || still[0].Mechanism != original.Mechanism {
		t.Fatalf("the candidate was altered by confirmation: %+v", still)
	}

	// A second click is a repeat, not a second human assertion.
	again, err := links.ConfirmLink(ctx, identityNS, original.ID, "operator:ada")
	if err != nil {
		t.Fatalf("re-confirm: %v", err)
	}
	if again.ID != confirmed.ID {
		t.Fatalf("re-confirm minted a second confirmation row (%s vs %s)", again.ID, confirmed.ID)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM links WHERE namespace_id=$1 AND mechanism=$2`,
		identityNS, domain.LinkMechanismHuman).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("%d human rows after two confirms, want 1", n)
	}
}

func TestPgLinkStore_RetractStampsTwoColumnsAndNoMore(t *testing.T) {
	_, links, pool, ctx := newIdentityStores(t)
	ev := seedEvidence(t, ctx, pool, "ev-retract")

	if _, err := links.PutLink(ctx, candidate(ev)); err != nil {
		t.Fatalf("put: %v", err)
	}
	before, err := links.Candidates(ctx, identityNS, 10)
	if err != nil || len(before) != 1 {
		t.Fatalf("Candidates: %v (%d)", err, len(before))
	}
	orig := before[0]

	if err := links.RetractLink(ctx, identityNS, orig.ID, "operator:ada", "wrong customer"); err != nil {
		t.Fatalf("retract: %v", err)
	}

	after, err := links.LinksFor(ctx, identityNS, orig.FromRef, domain.LinkQuery{IncludeRetracted: true})
	if err != nil || len(after) != 1 {
		t.Fatalf("LinksFor: %v (%d)", err, len(after))
	}
	got := after[0]
	if got.State != domain.LinkStateRetracted || got.RetractedAt == nil {
		t.Fatalf("retraction did not stamp state/retracted_at: %+v", got)
	}
	// EVERY other column identical. An existing row that can be rewritten is a row
	// whose history cannot be trusted (ADR-0093 D6) — in particular the retraction
	// actor must NOT have overwritten asserted_by.
	if got.AssertedBy != orig.AssertedBy {
		t.Fatalf("retraction overwrote asserted_by: %q → %q", orig.AssertedBy, got.AssertedBy)
	}
	if got.Mechanism != orig.Mechanism || got.Producer != orig.Producer ||
		got.Confidence != orig.Confidence || got.EvidenceID != orig.EvidenceID ||
		got.SourceRef != orig.SourceRef || got.Relation != orig.Relation ||
		got.FromRef != orig.FromRef || got.ToRef != orig.ToRef ||
		!got.AssertedAt.Equal(orig.AssertedAt) {
		t.Fatalf("retraction rewrote a column it must never touch:\n before %+v\n after  %+v", orig, got)
	}

	// A retracted candidate stays QUERYABLE and out of the inbox: a producer that
	// cannot see its proposal was rejected re-proposes it forever.
	pending, err := links.Candidates(ctx, identityNS, 10)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("a retracted link is still in the review inbox")
	}
	// Retracting twice is a no-op, not a fault.
	if err := links.RetractLink(ctx, identityNS, orig.ID, "operator:ada", "again"); err != nil {
		t.Fatalf("second retract: %v", err)
	}
}

func TestPgLinkStore_RetractByProducerRevokesTheBatch(t *testing.T) {
	_, links, pool, ctx := newIdentityStores(t)
	ev := seedEvidence(t, ctx, pool, "ev-batch")

	for i, ref := range []string{"customer/A", "customer/B", "customer/C"} {
		l := candidate(ev)
		l.FromRef = domain.EntityRef(ref)
		l.SourceRef = "orders/row-" + string(rune('0'+i))
		if _, err := links.PutLink(ctx, l); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	// A different producer's row must survive: the whole point of the column is
	// that one bad pass is undone WITHOUT collateral damage.
	keep := candidate(ev)
	keep.Producer = "mapping/orders@v3"
	keep.Mechanism = domain.LinkMechanismDeclared
	keep.SourceRef = "orders/row-keep"
	keep.FromRef = domain.EntityRef("customer/D")
	if _, err := links.PutLink(ctx, keep); err != nil {
		t.Fatalf("put keeper: %v", err)
	}

	n, err := links.RetractByProducer(ctx, identityNS, "id_shape@v1", "operator:ada")
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if n != 3 {
		t.Fatalf("revoked %d rows, want 3", n)
	}
	// Idempotent: nothing left unretracted to revoke.
	if n, err = links.RetractByProducer(ctx, identityNS, "id_shape@v1", "operator:ada"); err != nil || n != 0 {
		t.Fatalf("second revoke = %d, %v", n, err)
	}
	// An empty producer would match every row that never named one — refused.
	if _, err := links.RetractByProducer(ctx, identityNS, "", "operator:ada"); !errors.Is(err, domain.ErrLinkRefused) {
		t.Fatalf("an empty producer was accepted: %v", err)
	}
	var live int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM links WHERE namespace_id=$1 AND retracted_at IS NULL`,
		identityNS).Scan(&live); err != nil {
		t.Fatalf("count: %v", err)
	}
	if live != 1 {
		t.Fatalf("%d live links after the batch revocation, want 1 (the other producer's)", live)
	}
}

func TestPgLinkStore_CandidatesOrderByConfidence(t *testing.T) {
	_, links, pool, ctx := newIdentityStores(t)
	ev := seedEvidence(t, ctx, pool, "ev-order")

	for i, c := range []float64{0.4, 0.9, 0.6} {
		l := candidate(ev)
		l.Confidence = c
		l.FromRef = domain.EntityRef("customer/" + string(rune('A'+i)))
		l.SourceRef = "orders/row-" + string(rune('0'+i))
		if _, err := links.PutLink(ctx, l); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	got, err := links.Candidates(ctx, identityNS, 10)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d candidates, want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Confidence < got[i].Confidence {
			t.Fatalf("the review inbox is not highest-confidence-first: %v", got)
		}
	}
}

func TestPgEntityStore_MintIsIdempotentAndDerivesKind(t *testing.T) {
	entities, _, pool, ctx := newIdentityStores(t)
	ev := seedEvidence(t, ctx, pool, "ev-mint")

	created, err := entities.EnsureEntity(ctx, domain.Entity{
		ID: "customer/C-1042", NamespaceID: identityNS, FirstSeenEvidence: ev,
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !created {
		t.Fatalf("first mint reported created=false")
	}
	if created, err = entities.EnsureEntity(ctx, domain.Entity{ID: "customer/C-1042", NamespaceID: identityNS}); err != nil || created {
		t.Fatalf("re-mint = %v, %v — minting is not idempotent", created, err)
	}

	got, ok, err := entities.GetEntity(ctx, identityNS, "customer/C-1042")
	if err != nil || !ok {
		t.Fatalf("GetEntity: %v, ok=%v", err, ok)
	}
	// The kind is the PREFIX STEM; a producer holding "customer/C-1042" should not
	// have to say "customer" twice.
	if got.Kind != "customer" {
		t.Fatalf("kind = %q, want the prefix stem", got.Kind)
	}
	if got.FirstSeenEvidence != ev {
		t.Fatalf("first_seen_evidence = %q", got.FirstSeenEvidence)
	}

	// An unscoped id is the collision the prefix exists to prevent — a permanent
	// refusal, not a default kind.
	if _, err := entities.EnsureEntity(ctx, domain.Entity{ID: "C-1042", NamespaceID: identityNS}); !errors.Is(err, domain.ErrLinkRefused) {
		t.Fatalf("an unscoped id was minted: %v", err)
	}

	byKind, err := entities.ListEntitiesByKind(ctx, identityNS, "customer", 10)
	if err != nil {
		t.Fatalf("ListEntitiesByKind: %v", err)
	}
	if len(byKind) != 1 || byKind[0].ID != "customer/C-1042" {
		t.Fatalf("ListEntitiesByKind = %+v", byKind)
	}
	// "No such entity" is an answer, never an error.
	if _, ok, err := entities.GetEntity(ctx, identityNS, "customer/nope"); err != nil || ok {
		t.Fatalf("GetEntity(missing) = ok=%v, err=%v", ok, err)
	}
}

// ResolveEntityByLocal is the bare-token lookup (build doc P2, D-W2-2), and what it must
// get right is the arithmetic of the answer, not its ranking: ONE hit is a resolution,
// zero or many is an ambiguity the producer records instead of guessing at.
//
// The escaping case is here because it is the one that fails SILENTLY. An unescaped
// underscore is a LIKE wildcard, so "OPS_12" would match "OPS-12" — and the damage is not
// an error a caller sees, it is a confident reference link to the wrong entity, or a
// clean resolution turned into a spurious ambiguity.
func TestPgEntityStore_ResolveByLocalCountsHitsExactly(t *testing.T) {
	entities, _, pool, ctx := newIdentityStores(t)
	ev := seedEvidence(t, ctx, pool, "ev-resolve")
	for _, id := range []string{
		"ticket/OPS-0412", "order/OPS-0412", "ticket/OPS-9999",
		"ticket/OPS_12", "ticket/OPS-12",
	} {
		if _, err := entities.EnsureEntity(ctx, domain.Entity{
			ID: id, NamespaceID: identityNS, FirstSeenEvidence: ev,
		}); err != nil {
			t.Fatalf("mint %s: %v", id, err)
		}
	}

	// Many: the token names a local id two kinds both use, so it names neither.
	many, err := entities.ResolveEntityByLocal(ctx, identityNS, "OPS-0412", 8)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(many) != 2 {
		t.Fatalf("ambiguous local resolved to %d handles, want both kinds: %+v", len(many), many)
	}

	// One: the resolution the reference producer acts on.
	one, err := entities.ResolveEntityByLocal(ctx, identityNS, "OPS-9999", 8)
	if err != nil || len(one) != 1 || one[0].ID != "ticket/OPS-9999" {
		t.Fatalf("unique local resolved to %+v (err %v)", one, err)
	}

	// Zero is an answer, never an error.
	none, err := entities.ResolveEntityByLocal(ctx, identityNS, "OPS-0001", 8)
	if err != nil || len(none) != 0 {
		t.Fatalf("unknown local resolved to %+v (err %v)", none, err)
	}

	// The wildcard that is not a wildcard.
	lit, err := entities.ResolveEntityByLocal(ctx, identityNS, "OPS_12", 8)
	if err != nil {
		t.Fatalf("resolve escaped: %v", err)
	}
	if len(lit) != 1 || lit[0].ID != "ticket/OPS_12" {
		t.Fatalf("an underscore matched as a wildcard: %+v", lit)
	}

	// The match is right-ANCHORED at a kind boundary: "0412" is a suffix of
	// "OPS-0412" but is not its local id, and matching it would resolve every
	// token that happened to end the same way.
	if got, err := entities.ResolveEntityByLocal(ctx, identityNS, "0412", 8); err != nil || len(got) != 0 {
		t.Fatalf("a partial local matched: %+v (err %v)", got, err)
	}
}

func TestPgLinkStore_LinksForFiltersAndDirection(t *testing.T) {
	_, links, pool, ctx := newIdentityStores(t)
	ev := seedEvidence(t, ctx, pool, "ev-dir")

	// An ASYMMETRIC verb: direction is the meaning, so the inbound row appears only
	// when the caller asked for the backward walk.
	rel := domain.Link{
		NamespaceID: identityNS,
		FromRef:     domain.EntityRef("company/SUB"),
		ToRef:       domain.EntityRef("company/PARENT"),
		Relation:    "subsidiary_of",
		Mechanism:   domain.LinkMechanismRecord,
		State:       domain.LinkStateConfirmed,
		EvidenceID:  ev,
		AssertedBy:  "mapping/registry",
		SourceRef:   "registry/row-1",
	}
	if _, err := links.PutLink(ctx, rel); err != nil {
		t.Fatalf("put: %v", err)
	}
	// The family was left empty on purpose: the verb's declaration already names it.
	stored, err := links.LinksFor(ctx, identityNS, rel.FromRef, domain.LinkQuery{})
	if err != nil || len(stored) != 1 {
		t.Fatalf("LinksFor(from): %v (%d)", err, len(stored))
	}
	if stored[0].Family != domain.LinkFamilyRelation {
		t.Fatalf("family was not derived from the verb: %q", stored[0].Family)
	}

	if inbound, err := links.LinksFor(ctx, identityNS, rel.ToRef, domain.LinkQuery{}); err != nil || len(inbound) != 0 {
		t.Fatalf("an asymmetric verb answered for its TO endpoint: %v (%d)", err, len(inbound))
	}
	if inbound, err := links.LinksFor(ctx, identityNS, rel.ToRef, domain.LinkQuery{IncludeIncoming: true}); err != nil || len(inbound) != 1 {
		t.Fatalf("the backward walk found nothing: %v (%d)", err, len(inbound))
	}
	if none, err := links.LinksFor(ctx, identityNS, rel.FromRef, domain.LinkQuery{Relation: domain.RelationSameAs}); err != nil || len(none) != 0 {
		t.Fatalf("the relation filter leaked: %v (%d)", err, len(none))
	}
	if none, err := links.LinksFor(ctx, identityNS, rel.FromRef, domain.LinkQuery{State: domain.LinkStateCandidate}); err != nil || len(none) != 0 {
		t.Fatalf("the state filter leaked: %v (%d)", err, len(none))
	}
}
