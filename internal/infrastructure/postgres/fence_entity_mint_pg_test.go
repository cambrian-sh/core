package postgres

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0158 stage 5 (D4 b) against a real migrated Postgres (PG_TEST_DSN): an
// entity's mint is a typed row. Every first-seen read — the entity op's existence
// gate (mayKnowEntityExists), the EntityHandles listing, the identity scans
// (EntitiesSince, EntityByID, EntitiesByLocal, EntitiesWithParty and the mint
// route of PartiesForEntity) — fences the minting evidence AND the minting row's
// classification (entities.first_seen_classification), so an entity minted only
// by a row a mapping restricted is, for a reader the tag restricts, exactly a
// never-minted id. The minting evidence is readable by every reader here, so only
// the row classification can hide the entity.

const (
	emMinted = "ownleak/mint-hr"     // minted by a restricted row, nothing else refers to it
	emAbsent = "ownleak/never-there" // never minted
	emEv     = "ownleak-ev-mint-hr"  // readable minting evidence, carrying a party
	emParty  = "person:ownleak-mint"
	emKind   = "ownleak"
)

func (f *slFixture) mintRestricted(t *testing.T, rowTags []string) {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO evidence (id, namespace_id, source_id, source_key, content_hash, classification, parties)
		VALUES ($1,'default','ownleak-src-mint-hr',$1,'sha256:test','{mail}',$2)`,
		emEv, []string{emParty}); err != nil {
		t.Fatalf("minting evidence: %v", err)
	}
	if _, err := NewPgEntityStore(f.pool, nil).EnsureEntity(f.ctx, domain.Entity{
		ID: emMinted, NamespaceID: "default", FirstSeenEvidence: emEv,
		FirstSeenClassification: rowTags,
	}); err != nil {
		t.Fatalf("mint: %v", err)
	}
}

// mintSnapshot is every first-seen read of one reader, for the minted entity.
func (f *slFixture) mintSnapshot(t *testing.T, scope *domain.TagPredicate, id string) string {
	t.Helper()
	ctx := f.ctx
	var b strings.Builder
	put := func(label string, v any, err error) {
		fmt.Fprintf(&b, "%s: %v err=%v\n", label, v, err)
	}
	res, err := f.plane.Query(ctx, domain.KnowledgeQuery{Kind: domain.QueryEntity, EntityID: id}, scope)
	put("entity op rows", len(res.Rows), err)

	page, err := f.plane.Catalog(scope).EntityHandles(ctx, domain.EntityHandleFilter{Namespace: "default", AfterID: "ownleak/", Limit: 1000})
	var hs []string
	for _, h := range page.Handles {
		if h.ID == id {
			hs = append(hs, h.ID)
		}
	}
	put("listing handles", hs, err)

	scans := NewPgIdentityScanSource(f.pool)
	ids := func(rows []domain.IdentityEntityRow) []string {
		var out []string
		for _, r := range rows {
			if r.ID == id {
				out = append(out, r.ID)
			}
		}
		sort.Strings(out)
		return out
	}
	since, err := scans.EntitiesSince(ctx, scope, "default", domain.IdentityScanPosition{}, 100000)
	put("scan entities since", ids(since), err)
	_, exists, err := scans.EntityByID(ctx, scope, "default", id)
	put("scan entity exists", exists, err)
	local := id[strings.Index(id, "/")+1:]
	byLocal, err := scans.EntitiesByLocal(ctx, scope, "default", local, 10)
	put("scan by local", ids(byLocal), err)
	withParty, err := scans.EntitiesWithParty(ctx, scope, "default", emParty, emKind, 10)
	put("scan with party", ids(withParty), err)
	parties, err := scans.PartiesForEntity(ctx, scope, "default", id, 10)
	put("scan parties", parties, err)
	return b.String()
}

// An entity minted only by a restricted row is indistinguishable from a
// never-minted id for every reader the row's tag restricts, on every first-seen
// read, and is fully visible to a reader it does not restrict.
func TestPg_EntityMint_RestrictedMintIsAbsentForEveryFirstSeenRead(t *testing.T) {
	f := newSubstrateLeakFixture(t)
	f.mintRestricted(t, []string{trHRTag})
	absentWant := f.mintSnapshot(t, trGranted, emAbsent)
	for _, r := range []struct {
		name  string
		scope *domain.TagPredicate
	}{
		{"hr closed (not granted)", trNoGrant},
		{"hr party-scoped (not a party)", trNotParty},
		{"any-of [hr, finance] (would widen under a union fence)", trAnyOfHR},
	} {
		hidden := f.mintSnapshot(t, r.scope, emMinted)
		absent := f.mintSnapshot(t, r.scope, emAbsent)
		if hidden != absent {
			t.Errorf("%s: the restricted mint reads differently from a never-minted id:\n%s", r.name, diffLines(absent, hidden))
		}
		if absent != absentWant {
			t.Errorf("%s: a never-minted id reads differently from the granted reader's:\n%s", r.name, diffLines(absentWant, absent))
		}
	}
	granted := f.mintSnapshot(t, trGranted, emMinted)
	for _, want := range []string{
		"entity op rows: 1 ", "listing handles: [" + emMinted + "]", "scan entities since: [" + emMinted + "]",
		"scan entity exists: true", "scan by local: [" + emMinted + "]", "scan with party: [" + emMinted + "]",
		emParty,
	} {
		if !strings.Contains(granted, want) {
			t.Errorf("granted reader: missing %q in\n%s", want, granted)
		}
	}
}

// For R = ∅ the mint reads as before stage 5: the reader the [hr] mint was hidden
// from sees the unclassified mint exactly as the granted reader does (every
// production mint is in this case while ENG-1041 refuses classified mappings).
func TestPg_EntityMint_EmptyFirstSeenClassificationReadsAsToday(t *testing.T) {
	f := newSubstrateLeakFixture(t)
	f.mintRestricted(t, nil)
	granted := f.mintSnapshot(t, trGranted, emMinted)
	// The any-of reader cannot read the [mail] evidence at all: the mint is
	// hidden from it with R = ∅ as it is with R = [hr] (row tags never widen).
	if got, absent := f.mintSnapshot(t, trAnyOfHR, emMinted), f.mintSnapshot(t, trAnyOfHR, emAbsent); got != absent {
		t.Errorf("R = ∅: any-of reader sees a mint on evidence it cannot read:\n%s", diffLines(absent, got))
	}
	for _, r := range []*domain.TagPredicate{trNoGrant, trNotParty} {
		if got := f.mintSnapshot(t, r, emMinted); got != granted {
			t.Errorf("R = ∅: reader %+v differs from the granted reader:\n%s", r, diffLines(granted, got))
		}
	}
	if !strings.Contains(granted, "scan entity exists: true") {
		t.Errorf("R = ∅: the mint is invisible:\n%s", granted)
	}
}

// A later readable row reveals the entity legitimately (ADR-0158 D4: the other
// D12 branches still apply) — the restricted mint hides only what the mint
// alone would disclose.
func TestPg_EntityMint_ReadableRowStillRevealsTheEntity(t *testing.T) {
	f := newSubstrateLeakFixture(t)
	f.mintRestricted(t, []string{trHRTag})
	f.evidence(t, "ownleak-ev-mint-pub", "ownleak-src-mint-pub", []string{"mail"})
	if _, err := f.events.RecordObservation(f.ctx, domain.Observation{
		NamespaceID: "default", EntityID: emMinted, Predicate: "status",
		Value:      domain.StatementValue{Type: "number", Number: 1},
		OccurredAt: f.now, Confidence: 1, EvidenceID: "ownleak-ev-mint-pub",
		SourceRef: "ownleak-mint-pub-obs",
	}); err != nil {
		t.Fatalf("observation: %v", err)
	}
	point, err := f.plane.Query(f.ctx, domain.KnowledgeQuery{Kind: domain.QueryPoint, EntityID: emMinted, Predicate: "status"}, trNoGrant)
	if err != nil || len(point.Rows) != 1 {
		t.Errorf("a readable observation of a restricted-mint entity: rows=%v err=%v, want 1", point.Rows, err)
	}
	// The handle scans still answer by the mint only: no wider than before.
	if _, ok, err := NewPgIdentityScanSource(f.pool).EntityByID(f.ctx, trNoGrant, "default", emMinted); err != nil || ok {
		t.Errorf("EntityByID(restricted mint) = %v err=%v, want false", ok, err)
	}
}
