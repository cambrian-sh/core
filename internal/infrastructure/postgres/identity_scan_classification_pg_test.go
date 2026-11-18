package postgres

import (
	"reflect"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ENG-1999 against a real migrated Postgres (PG_TEST_DSN): every identity scan
// reads back the row classification its rows rest on, so a producer reading at
// ScopeSystem — which sees rows a mapping restricted — can stamp what it derives
// with those rows' tags (ADR-0158 D1). A read that reaches one fact by several
// routes reports the least restricted route, never a union of them.

const (
	icEv      = "ownleak-ev-icls"
	icParty   = "ownleak:icls-party"
	icHR      = "ownleak/icls-hr"     // alias, event role and mint: all [hr]
	icPub     = "ownleak/icls-pub"    // unclassified alias, unclassified mint
	icTwoWays = "ownleak/icls-twoway" // [hr] mint, unclassified observation on the same evidence
)

func (f *slFixture) seedIdentityClassification(t *testing.T) {
	t.Helper()
	at := f.now.Add(-time.Hour)
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO evidence (id, namespace_id, source_id, source_key, content_hash, classification, parties)
		VALUES ($1,'default','ownleak-src-icls',$1,'sha256:test','{mail}',$2)`,
		icEv, []string{icParty}); err != nil {
		t.Fatalf("evidence: %v", err)
	}
	ents := NewPgEntityStore(f.pool, nil)
	mint := func(id string, tags []string) {
		if _, err := ents.EnsureEntity(f.ctx, domain.Entity{
			ID: id, NamespaceID: "default", FirstSeenEvidence: icEv, FirstSeenClassification: tags,
		}); err != nil {
			t.Fatalf("mint %s: %v", id, err)
		}
	}
	mint(icHR, []string{"hr"})
	mint(icPub, nil)
	mint(icTwoWays, []string{"hr", "legal"})
	obs := func(entity, pred string, tags []string) {
		if _, err := f.events.RecordObservation(f.ctx, domain.Observation{
			NamespaceID: "default", EntityID: entity, Predicate: pred,
			Value:      domain.StatementValue{Type: "text", Text: "Icls Corp"},
			OccurredAt: at, Confidence: 1, EvidenceID: icEv,
			SourceRef: "ownleak-icls-" + entity + "-" + pred, Classification: tags,
		}); err != nil {
			t.Fatalf("observation %s: %v", entity, err)
		}
	}
	obs(icHR, "icls_alias", []string{"hr"})
	obs(icPub, "icls_alias", nil)
	obs(icTwoWays, "icls_status", nil)
	if _, _, err := f.events.RecordEvent(f.ctx, domain.Event{
		NamespaceID: "default", Type: "ownleak_icls_meeting", OccurredAt: at, EvidenceID: icEv,
		SourceRef: "ownleak-icls-event", Classification: []string{"hr"},
		Roles: []domain.EventRole{{Role: "r0", EntityID: icHR}, {Role: "r1", EntityID: icPub}},
	}); err != nil {
		t.Fatalf("event: %v", err)
	}
	if _, err := f.links.PutLink(f.ctx, domain.Link{
		NamespaceID: "default", Family: domain.LinkFamilyIdentity,
		FromRef: domain.EntityRef(icHR), ToRef: domain.EntityRef(icPub),
		Relation: domain.RelationSameAs, State: domain.LinkStateCandidate, Mechanism: domain.LinkMechanismDeclared,
		EvidenceID: icEv, AssertedBy: "test", Producer: "ownleak-icls@1",
		SourceRef: "ownleak-icls-link", Classification: []string{"hr"},
	}); err != nil {
		t.Fatalf("link: %v", err)
	}
}

func TestPg_IdentityScan_RowsCarryTheirRowClassification(t *testing.T) {
	f := newSubstrateLeakFixture(t)
	f.seedIdentityClassification(t)
	scans := NewPgIdentityScanSource(f.pool)
	ctx, sc := f.ctx, domain.ScopeSystem
	hr := []string{"hr"}
	eq := func(what string, got, want []string) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: classification = %#v, want %#v", what, got, want)
		}
	}

	aliases, err := scans.AliasObservations(ctx, sc, "default", []string{"icls_alias"}, 0, 100)
	if err != nil || len(aliases) != 2 {
		t.Fatalf("alias observations: %v err=%v, want 2", aliases, err)
	}
	for _, a := range aliases {
		switch a.EntityID {
		case icHR:
			eq("restricted alias", a.Classification, hr)
		case icPub:
			eq("unclassified alias", a.Classification, nil)
		}
	}

	since, err := scans.EntitiesSince(ctx, sc, "default", domain.IdentityScanPosition{}, 100000)
	if err != nil {
		t.Fatalf("entities since: %v", err)
	}
	seen := map[string][]string{}
	for _, e := range since {
		seen[e.ID] = e.Classification
	}
	eq("entities since: restricted mint", seen[icHR], hr)
	eq("entities since: unclassified mint", seen[icPub], nil)

	row, ok, err := scans.EntityByID(ctx, sc, "default", icHR)
	if err != nil || !ok {
		t.Fatalf("entity by id: ok=%v err=%v", ok, err)
	}
	eq("entity by id", row.Classification, hr)
	byLocal, err := scans.EntitiesByLocal(ctx, sc, "default", "icls-hr", 8)
	if err != nil || len(byLocal) != 1 {
		t.Fatalf("by local: %v err=%v", byLocal, err)
	}
	eq("entities by local", byLocal[0].Classification, hr)

	// Parties: icHR reaches the evidence by its [hr] mint, its [hr] alias and the
	// [hr] event — every route is restricted, so the party row is too. icTwoWays
	// reaches it by an [hr legal] mint AND an unclassified observation: the
	// unclassified route already discloses the fact, so the row is unrestricted.
	parties, err := scans.PartiesForEntity(ctx, sc, "default", icHR, 8)
	if err != nil || len(parties) != 1 {
		t.Fatalf("parties of the restricted entity: %v err=%v", parties, err)
	}
	eq("parties: every route restricted", parties[0].Classification, hr)
	parties, err = scans.PartiesForEntity(ctx, sc, "default", icTwoWays, 8)
	if err != nil || len(parties) != 1 {
		t.Fatalf("parties of the two-route entity: %v err=%v", parties, err)
	}
	eq("parties: least restricted route", parties[0].Classification, nil)

	// EntitiesWithParty: the route's tags plus the partner's own mint tags.
	partners, err := scans.EntitiesWithParty(ctx, sc, "default", icParty, "ownleak", 16)
	if err != nil {
		t.Fatalf("entities with party: %v", err)
	}
	seen = map[string][]string{}
	for _, e := range partners {
		seen[e.ID] = e.Classification
	}
	eq("with party: restricted routes and mint", seen[icHR], hr)
	eq("with party: unclassified", seen[icPub], nil)
	eq("with party: open route, restricted mint", seen[icTwoWays], []string{"hr", "legal"})

	events, err := scans.EventsSince(ctx, sc, "default", domain.IdentityScanPosition{}, 100000)
	if err != nil {
		t.Fatalf("events since: %v", err)
	}
	var evID string
	for _, e := range events {
		if e.EventType == "ownleak_icls_meeting" {
			evID = e.ID
			eq("events since", e.Classification, hr)
		}
	}
	if evID == "" {
		t.Fatal("the restricted event was not scanned at ScopeSystem")
	}
	prior, err := scans.PriorEventsSharingEntity(ctx, sc, "default", []string{icHR},
		domain.IdentityScanPosition{At: f.now, ID: "~"}, f.now.Add(-24*time.Hour), 8)
	if err != nil || len(prior) != 1 {
		t.Fatalf("prior events: %v err=%v", prior, err)
	}
	eq("prior events", prior[0].Classification, hr)

	cands, err := scans.CandidateLinksSince(ctx, sc, "default", []string{domain.LinkMechanismDeclared}, domain.IdentityScanPosition{}, 100000)
	if err != nil {
		t.Fatalf("candidate links: %v", err)
	}
	found := false
	for _, l := range cands {
		if l.SourceRef == "ownleak-icls-link" {
			found = true
			eq("candidate links", l.Classification, hr)
		}
	}
	if !found {
		t.Fatal("the restricted candidate was not scanned at ScopeSystem")
	}
}
