package postgres

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0158 stage 3 against a real migrated Postgres (PG_TEST_DSN): every typed-row
// read of the query plane, the catalog, the listings and the identity scans honours
// the row's OWN classification R through the narrow-only fence fence(E) AND
// fence(E ∪ R) (sqlfence.TypedRowExists in SQL, rowfence.TypedRow in the walks).
//
// The rows under test sit on PUBLIC evidence (tags [mail], the same evidence row the
// public corpus uses), so the evidence fence alone would show them to everyone; only
// their row classification [hr] can hide them. The central property is the same
// equality fence_substrate_reads_pg_test.go states for owner rows: every answer a
// reader who may not read hr gets — point, history, aggregate, events, traverse,
// entity and alias closure, why with correlation, the review inbox, every catalog
// read, the listings and the identity scans — is byte-identical before and after
// the restricted rows are written. A reader granted hr sees them (the fence narrows,
// it does not delete), and a reader whose any-of clause names hr gains nothing
// (row tags can never widen: the trap of a plain union fence, ADR-0158 alt. 1).
//
// The file name sorts before migration_guard_test.go on purpose (that test drops
// and recreates the public schema).

const (
	trHROnly  = "ownleak/hr-only"
	trAliasHR = "ownleak/alias-hr"
	trCandHR  = "ownleak/cand-hr"
	trHRType  = "ownleak_hr_meeting"
	trHRPred  = "ownleak_hr_salary"
	trHRAlias = "ownleak_hr_alias" // a text predicate, so the alias scan reads it
	trHRTag   = "hr"
	trEvPub   = "ownleak-ev-pub" // seedShared's public evidence
	trHRSrc   = "ownleak-hr-"    // source_ref prefix of every restricted row
	trHRLinks = "ownleak-hr@1"   // producer of the restricted links
)

var (
	// hr is closed for this reader: rendered as a forbidden term (ADR-0091).
	trNoGrant = &domain.TagPredicate{Owners: []string{"bob"}, ForbiddenTags: []string{trHRTag}}
	// hr is party-scoped and the reader is not among the (empty) parties.
	trNotParty = &domain.TagPredicate{Owners: []string{"bob"}, PartyScopedTags: []string{trHRTag},
		PartyIdentities: []string{"person:bob"}}
	// Would be WIDENED by a plain union fence: [mail] fails the any-of clause,
	// [mail, hr] passes it.
	trAnyOfHR = &domain.TagPredicate{Owners: []string{"bob"}, AnyOfClauses: [][]string{{trHRTag, "finance"}}}
	// Granted: nothing forbids hr.
	trGranted = &domain.TagPredicate{Owners: []string{"bob"}}
)

// seedRestricted writes, on the public evidence, rows whose row classification is
// [hr]: a newer value for the shared key, a value only they carry, an event the
// shared entity took part in, an alias and a candidate of the shared entity, and
// a value on the alias. Each one is a path a fence that ignored R would leak by.
func (f *slFixture) seedRestricted(t *testing.T, tags []string) {
	t.Helper()
	at := f.now.Add(-30 * time.Minute) // newer than every public row
	obs := func(entity, pred string, v float64) {
		if _, err := f.events.RecordObservation(f.ctx, domain.Observation{
			NamespaceID: "default", EntityID: entity, Predicate: pred,
			Value:      domain.StatementValue{Type: "number", Number: v},
			OccurredAt: at, Confidence: 1, EvidenceID: trEvPub,
			SourceRef: trHRSrc + entity + "-" + pred, Classification: tags,
		}); err != nil {
			t.Fatalf("observation %s/%s: %v", entity, pred, err)
		}
	}
	obs(slShared, "status", 3)
	obs(trHROnly, trHRPred, 99)
	obs(trAliasHR, "status", 8)
	if _, err := f.events.RecordObservation(f.ctx, domain.Observation{
		NamespaceID: "default", EntityID: trHROnly, Predicate: trHRAlias,
		Value:      domain.StatementValue{Type: "text", Text: "ownleak-hr-handle"},
		OccurredAt: at, Confidence: 1, EvidenceID: trEvPub,
		SourceRef: trHRSrc + trHROnly + "-" + trHRAlias, Classification: tags,
	}); err != nil {
		t.Fatalf("alias observation: %v", err)
	}
	if _, _, err := f.events.RecordEvent(f.ctx, domain.Event{
		NamespaceID: "default", Type: trHRType, OccurredAt: at, EvidenceID: trEvPub,
		SourceRef: trHRSrc + "event", Classification: tags,
		Roles: []domain.EventRole{{Role: "r0", EntityID: trHROnly}, {Role: "r1", EntityID: slShared}},
	}); err != nil {
		t.Fatalf("event: %v", err)
	}
	link := func(to, state, mechanism string) {
		if _, err := f.links.PutLink(f.ctx, domain.Link{
			NamespaceID: "default", Family: domain.LinkFamilyIdentity,
			FromRef: domain.EntityRef(slShared), ToRef: domain.EntityRef(to),
			Relation: domain.RelationSameAs, State: state, Mechanism: mechanism,
			EvidenceID: trEvPub, AssertedBy: "test", Producer: trHRLinks,
			SourceRef: trHRSrc + to, Classification: tags,
		}); err != nil {
			t.Fatalf("link →%s: %v", to, err)
		}
	}
	link(trAliasHR, domain.LinkStateConfirmed, domain.LinkMechanismDeclared)
	link(trCandHR, domain.LinkStateCandidate, domain.LinkMechanismScored)
}

func (f *slFixture) eventID(t *testing.T, sourceRef string) string {
	t.Helper()
	var id string
	if err := f.pool.QueryRow(f.ctx, `SELECT id FROM events WHERE source_ref = $1`, sourceRef).Scan(&id); err != nil {
		t.Fatalf("event id of %s: %v", sourceRef, err)
	}
	return id
}

// typedSnapshot is snapshot (every query-plane and catalog read) plus the reads
// this stage moved that snapshot does not cover: aggregate, traverse, why with
// correlation (from the entity and from the public event), the listings and the
// identity scans.
func (f *slFixture) typedSnapshot(t *testing.T, scope *domain.TagPredicate, pubEvent string) string {
	t.Helper()
	ctx := f.ctx
	var b strings.Builder
	b.WriteString(f.snapshot(t, scope))
	put := func(label string, v any, err error) {
		fmt.Fprintf(&b, "%s: %v err=%v\n", label, v, err)
	}
	from, to := f.now.Add(-24*time.Hour), f.now.Add(time.Hour)
	for _, q := range []domain.KnowledgeQuery{
		{Kind: domain.QueryAggregate, EntityID: slShared, Predicate: "status", Aggregate: "count", From: from, To: to},
		{Kind: domain.QueryAggregate, EntityID: slShared, Predicate: "status", Aggregate: "max", From: from, To: to},
		{Kind: domain.QueryTraverse, EntityID: slShared, Hops: 2, From: from, To: to},
		{Kind: domain.QueryWhy, EntityID: domain.EntityRef(slShared), IncludeCorrelation: true},
		{Kind: domain.QueryWhy, EntityID: domain.RefPrefixEvent + pubEvent, IncludeCorrelation: true},
	} {
		res, err := f.plane.Query(ctx, q, scope)
		put(fmt.Sprintf("query %s %s %s", q.Kind, q.EntityID, q.Aggregate), fmt.Sprintf("rows=%v", res.Rows), err)
	}
	cat := f.plane.Catalog(scope)
	links, err := cat.Links(ctx, domain.LinkListFilter{Namespace: "default", Limit: 1000})
	var lrefs []string
	for _, l := range links.Links {
		lrefs = append(lrefs, l.FromRef+"→"+l.ToRef+"/"+l.State)
	}
	sort.Strings(lrefs)
	put("listing links", lrefs, err)
	evs, err := cat.Events(ctx, domain.EventListFilter{Namespace: "default", SourceRefPrefix: "ownleak-", Limit: 1000})
	put("listing events", fmt.Sprintf("%+v", evs), err)

	scans := NewPgIdentityScanSource(f.pool)
	aliases, err := scans.AliasObservations(ctx, scope, "default", []string{trHRAlias}, 0, 1000)
	var arows []string
	for _, a := range aliases {
		arows = append(arows, a.EntityID+"="+a.Value)
	}
	sort.Strings(arows)
	put("scan aliases", arows, err)
	since, err := scans.EventsSince(ctx, scope, "default", domain.IdentityScanPosition{}, 1000)
	put("scan events since", eventTypes(since), err)
	prior, err := scans.PriorEventsSharingEntity(ctx, scope, "default", []string{slShared, trHROnly},
		domain.IdentityScanPosition{At: f.now.Add(time.Hour), ID: "~"}, from, 1000)
	put("scan prior events", eventTypes(prior), err)
	confirmed, err := scans.ConfirmedIdentityLinks(ctx, scope, "default", 1000)
	put("scan confirmed links", linkEnds(confirmed), err)
	cands, err := scans.CandidateLinksSince(ctx, scope, "default", nil, domain.IdentityScanPosition{}, 1000)
	put("scan candidate links", linkEnds(cands), err)
	return b.String()
}

func eventTypes(rows []domain.IdentityEventRow) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.EventType)
	}
	sort.Strings(out)
	return out
}

func linkEnds(links []domain.Link) []string {
	var out []string
	for _, l := range links {
		out = append(out, l.FromRef+"→"+l.ToRef)
	}
	sort.Strings(out)
	return out
}

// A row classification hides the row from every reader the tag restricts, on
// every typed-row read path, and leaves no trace in any count, window, discovery
// list, listing or scan; it never widens what a reader sees.
func TestPg_TypedRowReads_RowClassificationNarrowsEveryReadPath(t *testing.T) {
	f := newSubstrateLeakFixture(t)
	f.seedShared(t)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(f.ctx, `DELETE FROM links WHERE producer = $1`, trHRLinks) // best effort: also matched by the fixture's own cleanup
	})
	pubEvent := f.eventID(t, "ownleak-"+trEvPub+"-"+slPubType)
	readers := []struct {
		name  string
		scope *domain.TagPredicate
	}{
		{"hr closed (not granted)", trNoGrant},
		{"hr party-scoped (not a party)", trNotParty},
		{"any-of [hr, finance] (would widen under a union fence)", trAnyOfHR},
	}
	before := map[string]string{}
	for _, r := range readers {
		before[r.name] = f.typedSnapshot(t, r.scope, pubEvent)
	}
	grantedBefore := f.typedSnapshot(t, trGranted, pubEvent)

	f.seedRestricted(t, []string{trHRTag})
	hrEvent := f.eventID(t, trHRSrc+"event")

	for _, r := range readers {
		after := f.typedSnapshot(t, r.scope, pubEvent)
		if after != before[r.name] {
			t.Errorf("%s: rows classified [hr] changed this reader's answers.\n%s",
				r.name, diffLines(before[r.name], after))
		}
		for _, s := range []string{trHROnly, trAliasHR, trCandHR, trHRType, trHRPred, hrEvent, "ownleak-hr-handle"} {
			if strings.Contains(after, s) && !strings.Contains(before[r.name], s) {
				t.Errorf("%s: answer names %q", r.name, s)
			}
		}
		// The restricted event's participants (RoleEntities): none.
		if ents, err := NewPgIdentityScanSource(f.pool).RoleEntities(f.ctx, r.scope, "default", hrEvent, 10); err != nil || len(ents) != 0 {
			t.Errorf("%s: RoleEntities(restricted event) = %v err=%v", r.name, ents, err)
		}
		// The restricted event itself: hidden reads exactly like an absent id.
		hidden, herr := f.plane.Query(f.ctx, domain.KnowledgeQuery{Kind: domain.QueryWhy,
			EntityID: domain.RefPrefixEvent + hrEvent, IncludeCorrelation: true}, r.scope)
		absent, aerr := f.plane.Query(f.ctx, domain.KnowledgeQuery{Kind: domain.QueryWhy,
			EntityID: domain.RefPrefixEvent + "ownleak-no-such-event", IncludeCorrelation: true}, r.scope)
		if fmt.Sprint(hidden.Rows, herr) != fmt.Sprint(absent.Rows, aerr) {
			t.Errorf("%s: why(hidden event) %v/%v differs from why(absent event) %v/%v",
				r.name, hidden.Rows, herr, absent.Rows, aerr)
		}
	}

	// The granted reader sees every restricted row: the fence narrowed, it did
	// not delete.
	grantedAfter := f.typedSnapshot(t, trGranted, pubEvent)
	if grantedAfter == grantedBefore {
		t.Fatal("granted reader: the restricted rows are invisible to a reader hr does not restrict")
	}
	for _, s := range []string{trHROnly, trAliasHR, trCandHR, trHRType, trHRPred, hrEvent, "ownleak-hr-handle"} {
		if !strings.Contains(grantedAfter, s) {
			t.Errorf("granted reader: answer never names %q", s)
		}
	}
	point, err := f.plane.Query(f.ctx, domain.KnowledgeQuery{Kind: domain.QueryPoint, EntityID: slShared, Predicate: "status"}, trGranted)
	if err != nil || len(point.Rows) != 1 || point.Rows[0]["value_number"] != float64(3) {
		t.Errorf("granted point: rows=%v err=%v, want the newer restricted value 3", point.Rows, err)
	}
	point, err = f.plane.Query(f.ctx, domain.KnowledgeQuery{Kind: domain.QueryPoint, EntityID: slShared, Predicate: "status"}, trNoGrant)
	if err != nil || len(point.Rows) != 1 || point.Rows[0]["value_number"] != float64(1) {
		t.Errorf("not-granted point: rows=%v err=%v, want the latest VISIBLE value 1", point.Rows, err)
	}
	if roles, err := NewPgIdentityScanSource(f.pool).RoleEntities(f.ctx, trGranted, "default", hrEvent, 10); err != nil || len(roles) != 2 {
		t.Errorf("granted RoleEntities(restricted event) = %v err=%v, want both participants", roles, err)
	}
	ents, err := f.plane.Catalog(trGranted).Entities(f.ctx, domain.EntityFilter{EventType: trHRType})
	if err != nil || len(ents) != 2 {
		t.Errorf("granted catalog by event type: %+v err=%v, want both participants", ents, err)
	}
}

// For R = ∅ nothing changes: the same rows written with no row classification
// are visible to the reader that the [hr] rows were hidden from (ENG-1041 still
// refuses a classified mapping, so every production row is in this case).
func TestPg_TypedRowReads_EmptyRowClassificationReadsAsToday(t *testing.T) {
	f := newSubstrateLeakFixture(t)
	f.seedShared(t)
	f.seedRestricted(t, nil)
	pubEvent := f.eventID(t, "ownleak-"+trEvPub+"-"+slPubType)
	a := f.typedSnapshot(t, trNoGrant, pubEvent)
	b := f.typedSnapshot(t, trGranted, pubEvent)
	if a != b {
		t.Errorf("with R = ∅, forbidding a tag no row carries changed the answers:\n%s", diffLines(b, a))
	}
	for _, s := range []string{trHROnly, trAliasHR, trCandHR, trHRType, trHRPred} {
		if !strings.Contains(a, s) {
			t.Errorf("R = ∅: answer never names %q", s)
		}
	}
}

// PartiesForEntity / EntitiesWithParty (ADR-0158 D3 sites 33-34): a restricted
// observation or event lends its evidence's parties to no reader it is hidden from.
func TestPg_TypedRowReads_RestrictedRowsLendNoParties(t *testing.T) {
	f := newSubstrateLeakFixture(t)
	const party, kind = "person:ownleak-p1", "ownleak"
	// The entity is minted by readable evidence (first-seen is stage 5's matter).
	f.evidence(t, "ownleak-ev-mint", "ownleak-src-mint", []string{"mail"})
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO evidence (id, namespace_id, source_id, source_key, content_hash, classification, parties)
		VALUES ('ownleak-ev-party','default','ownleak-src-party','ownleak-ev-party','sha256:test','{mail}',$1)`,
		[]string{party}); err != nil {
		t.Fatalf("party evidence: %v", err)
	}
	if _, err := NewPgEntityStore(f.pool, nil).EnsureEntity(f.ctx, domain.Entity{
		ID: trHROnly, NamespaceID: "default", FirstSeenEvidence: "ownleak-ev-mint",
	}); err != nil {
		t.Fatalf("entity: %v", err)
	}
	if _, err := f.events.RecordObservation(f.ctx, domain.Observation{
		NamespaceID: "default", EntityID: trHROnly, Predicate: trHRPred,
		Value:      domain.StatementValue{Type: "number", Number: 1},
		OccurredAt: f.now, Confidence: 1, EvidenceID: "ownleak-ev-party",
		SourceRef: trHRSrc + "party-obs", Classification: []string{trHRTag},
	}); err != nil {
		t.Fatalf("observation: %v", err)
	}
	if _, _, err := f.events.RecordEvent(f.ctx, domain.Event{
		NamespaceID: "default", Type: trHRType, OccurredAt: f.now, EvidenceID: "ownleak-ev-party",
		SourceRef: trHRSrc + "party-event", Classification: []string{trHRTag},
		Roles: []domain.EventRole{{Role: "r0", EntityID: trHROnly}},
	}); err != nil {
		t.Fatalf("event: %v", err)
	}
	scans := NewPgIdentityScanSource(f.pool)
	for _, tc := range []struct {
		name  string
		scope *domain.TagPredicate
		want  bool
	}{
		{"not granted", trNoGrant, false},
		{"any-of [hr, finance]", trAnyOfHR, false},
		{"granted", trGranted, true},
	} {
		ps, err := scans.PartiesForEntity(f.ctx, tc.scope, "default", trHROnly, 10)
		if err != nil || (len(ps) == 1 && ps[0].Party == party) != tc.want || (!tc.want && len(ps) != 0) {
			t.Errorf("%s: PartiesForEntity = %+v err=%v, want visible=%v", tc.name, ps, err, tc.want)
		}
		es, err := scans.EntitiesWithParty(f.ctx, tc.scope, "default", party, kind, 10)
		if err != nil || (len(es) == 1 && es[0].ID == trHROnly) != tc.want || (!tc.want && len(es) != 0) {
			t.Errorf("%s: EntitiesWithParty = %+v err=%v, want visible=%v", tc.name, es, err, tc.want)
		}
	}
}
