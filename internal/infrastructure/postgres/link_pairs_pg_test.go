package postgres

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// The review lane over CLAIMS (ENG-2030; owner decisions 2026-11-18) and the
// supporting-evidence erasure cascade (ENG-2032), against a real, migrated
// Postgres (PG_TEST_DSN, through 0056).

func pairRow(ev domain.EvidenceID, source string, conf float64, class []string, supporting ...domain.EvidenceID) domain.Link {
	l := inboxCandidate(ev, "person/E-17", "person/P", source, conf, time.Now().UTC().Add(-time.Hour))
	l.Mechanism = domain.LinkMechanismDerived
	l.Classification = class
	l.SupportingEvidence = supporting
	return l
}

func pairKeyEP() domain.LinkPairKey {
	return domain.LinkPairKey{NamespaceID: identityNS, Family: domain.LinkFamilyIdentity,
		FromRef: domain.EntityRef("person/E-17"), ToRef: domain.EntityRef("person/P"), Relation: domain.RelationSameAs}
}

func sourceRefs(ls []domain.Link) []string {
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.SourceRef)
	}
	slices.Sort(out)
	return out
}

func TestPgLinkStore_PairsGroupConfirmAndReject(t *testing.T) {
	_, links, pool, ctx := newIdentityStores(t)
	evHR := seedEvidence(t, ctx, pool, "ev-pair-payroll")
	evDir := seedEvidence(t, ctx, pool, "ev-pair-directory")
	evDir2 := seedEvidence(t, ctx, pool, "ev-pair-directory-2")
	evOther := seedEvidence(t, ctx, pool, "ev-pair-other")

	// Two supporting sources of one claim (an hr row and an open row) and an
	// unrelated claim. The open row records the directory row it was derived
	// from besides its cited evidence.
	for _, l := range []domain.Link{
		pairRow(evHR, "lane:hr", 0.8, []string{"hr"}, evDir),
		pairRow(evDir2, "lane:open", 0.6, nil, evDir, evDir, ""),
		inboxCandidate(evOther, "person/X", "person/Y", "other", 0.7, time.Now().UTC()),
	} {
		if _, err := links.PutLink(ctx, l); err != nil {
			t.Fatalf("put: %v", err)
		}
	}

	pairs, err := links.CandidatePairs(ctx, identityNS, 10)
	if err != nil {
		t.Fatalf("CandidatePairs: %v", err)
	}
	if len(pairs) != 2 {
		t.Fatalf("pairs = %d, want the two claims once each", len(pairs))
	}
	if pairs[0].Key != pairKeyEP() || len(pairs[0].Candidates) != 2 {
		t.Fatalf("first pair %+v, want E-17=P (best row 0.8) with both rows", pairs[0].Key)
	}
	for _, c := range pairs[0].Candidates {
		if c.SourceRef == "lane:open" && !slices.Equal(c.SupportingEvidence, []domain.EvidenceID{evDir}) {
			t.Errorf("supporting evidence round-trip = %v, want [%s] (sorted, unique, no empties, not the cited one)", c.SupportingEvidence, evDir)
		}
	}

	// Pair-level confirm: one confirmation per row, each with that row's
	// evidence, classification and supporting evidence.
	ids := []string{pairs[0].Candidates[0].ID, pairs[0].Candidates[1].ID}
	conf, err := links.ConfirmLinks(ctx, identityNS, ids, "operator:ada")
	if err != nil || len(conf) != 2 {
		t.Fatalf("ConfirmLinks: %v (%d)", err, len(conf))
	}
	for i, c := range conf {
		src := pairs[0].Candidates[i]
		if c.Mechanism != domain.LinkMechanismHuman || c.SourceRef != "confirm:"+src.ID || c.EvidenceID != src.EvidenceID ||
			!slices.Equal(c.Classification, src.Classification) || !slices.Equal(c.SupportingEvidence, src.SupportingEvidence) {
			t.Errorf("confirmation %d = %+v, want it to inherit %+v", i, c, src)
		}
	}
	after, err := links.PairLinks(ctx, pairKeyEP())
	if err != nil || len(after.Candidates) != 0 || len(after.Confirmed) != 2 {
		t.Fatalf("after confirm: %v candidates=%d confirmed=%d", err, len(after.Candidates), len(after.Confirmed))
	}

	// A later source after the confirmation is a candidate again, and the pair
	// carries its confirmations ("extend the confirmation?").
	if _, err := links.PutLink(ctx, pairRow(evOther, "lane:later", 0.5, nil)); err != nil {
		t.Fatal(err)
	}
	later, err := links.PairLinks(ctx, domain.LinkPairKey{NamespaceID: identityNS, Family: domain.LinkFamilyIdentity,
		FromRef: domain.EntityRef("person/P"), ToRef: domain.EntityRef("person/E-17"), Relation: domain.RelationSameAs})
	if err != nil || sourceRefs(later.Candidates)[0] != "lane:later" || len(later.Confirmed) != 2 {
		t.Fatalf("later source: %v %v confirmed=%d (reversed endpoints must name the same pair)", err, sourceRefs(later.Candidates), len(later.Confirmed))
	}

	// A refusal of one row rolls the whole pair confirm back.
	if _, err := links.ConfirmLinks(ctx, identityNS, []string{later.Candidates[0].ID, "lnk_missing"}, "operator:ada"); !errors.Is(err, domain.ErrLinkRefused) {
		t.Fatalf("ConfirmLinks with an unknown row: %v, want ErrLinkRefused", err)
	}
	if again, _ := links.PairLinks(ctx, pairKeyEP()); len(again.Candidates) != 1 || len(again.Confirmed) != 2 {
		t.Fatalf("a refused pair confirm left %d candidates / %d confirmations", len(again.Candidates), len(again.Confirmed))
	}

	// Pair-level reject: retracts the given rows, records the claim's rejection
	// (born retracted, no evidence), and every LATER candidate of the claim stays
	// out of both inbox reads. Idempotent.
	n, err := links.RejectLinkPair(ctx, pairKeyEP(), []string{later.Candidates[0].ID}, "operator:ada")
	if err != nil || n != 1 {
		t.Fatalf("RejectLinkPair: %d %v", n, err)
	}
	if n, err := links.RejectLinkPair(ctx, pairKeyEP(), []string{later.Candidates[0].ID}, "operator:ada"); err != nil || n != 0 {
		t.Fatalf("second reject: %d %v, want a no-op", n, err)
	}
	var rejections int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM links WHERE namespace_id=$1 AND mechanism='human'
		AND source_ref LIKE 'reject:%' AND state='retracted' AND retracted_at IS NOT NULL AND evidence_id IS NULL`,
		identityNS).Scan(&rejections); err != nil || rejections != 1 {
		t.Fatalf("rejection rows = %d (%v), want exactly one born-retracted, evidence-less row", rejections, err)
	}
	ev2 := seedEvidence(t, ctx, pool, "ev-pair-after-reject")
	if _, err := links.PutLink(ctx, pairRow(ev2, "lane:after-reject", 0.99, nil)); err != nil {
		t.Fatal(err)
	}
	inbox, _ := links.Candidates(ctx, identityNS, 50)
	for _, l := range inbox {
		if domain.PairKeyOf(l) == pairKeyEP() {
			t.Fatalf("a rejected claim's later candidate %q is in the row inbox", l.SourceRef)
		}
	}
	pairs, _ = links.CandidatePairs(ctx, identityNS, 10)
	if len(pairs) != 1 || pairs[0].Key.FromRef != domain.EntityRef("person/X") {
		t.Fatalf("pair inbox after reject = %+v, want only the unrelated claim", pairs)
	}
	lane, _ := links.CandidatesForRefs(ctx, identityNS, []string{domain.EntityRef("person/P")}, 0, 10)
	if len(lane[domain.EntityRef("person/P")]) != 0 {
		t.Fatalf("the entity card still lists the rejected claim's candidates")
	}
	// The rejection row is what the producers' suppression check reads.
	all, _ := links.LinksFor(ctx, identityNS, domain.EntityRef("person/P"), domain.LinkQuery{IncludeRetracted: true, IncludeIncoming: true})
	if !slices.ContainsFunc(all, domain.IsPairRejection) {
		t.Fatal("LinksFor(IncludeRetracted) does not return the pair rejection")
	}
}

// ENG-2032: erasing a supporting row that is NOT the cited evidence removes the
// link and its confirmation (the confirmation inherited the supporting set), so
// the restriction returns on its own: a reader without the payroll grant, who
// reached the pair only through the directory row, no longer does; a payroll
// reader still reaches it through the payroll row, which never rested on the
// erased row.
func TestPg_Erase_RemovesLinksDerivedFromASupportingRow(t *testing.T) {
	a, ks, er, ctx := newErasureStore(t)
	for _, doc := range []string{"lctest-pay", "lctest-dir1", "lctest-dir2"} {
		seedLifecycleDoc(t, a, ks, ctx, doc, "agent:a", "row of "+doc, "dg-"+doc, 1)
		seedErasureEvidence(t, a, ctx, doc)
	}
	links := NewPgLinkStore(a.pool, nil)
	plane := NewPgQueryPlane(a.pool, NewPgEventStore(a.pool, nil), nil, nil, links, NewPgEntityStore(a.pool, nil), nil)
	const ns = "lctest"
	t.Cleanup(func() { execSQL(t, a, ctx, `DELETE FROM links WHERE namespace_id=$1`, ns) })
	base := domain.Link{NamespaceID: ns, Family: domain.LinkFamilyIdentity,
		FromRef: domain.EntityRef("person/lctest-a"), ToRef: domain.EntityRef("person/lctest-b"),
		Relation: domain.RelationSameAs, Mechanism: domain.LinkMechanismDerived, Producer: "lctest@1",
		AssertedBy: "producer:lctest"}
	payrollRow := base
	payrollRow.EvidenceID, payrollRow.Classification, payrollRow.SourceRef = "lctest-pay-ev", []string{"payroll"}, "lctest:payroll"
	openRow := base
	openRow.EvidenceID, openRow.SupportingEvidence, openRow.SourceRef = "lctest-dir2-ev", []domain.EvidenceID{"lctest-dir1-ev"}, "lctest:open"
	for _, l := range []domain.Link{payrollRow, openRow} {
		if _, err := links.PutLink(ctx, l); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	key := domain.PairKeyOf(base)
	denied := &domain.TagPredicate{ForbiddenTags: []string{"payroll"}}
	granted := &domain.TagPredicate{}
	readable := func(scope *domain.TagPredicate) (cands, confirmed int) {
		p, err := plane.ReadablePair(ctx, key, scope)
		if err != nil {
			t.Fatalf("readable pair: %v", err)
		}
		return len(p.Candidates), len(p.Confirmed)
	}
	if c, _ := readable(denied); c != 1 {
		t.Fatalf("before erasure the non-payroll reader reaches the pair through %d row(s), want the directory row", c)
	}
	if c, _ := readable(granted); c != 2 {
		t.Fatalf("before erasure the payroll reader reaches %d row(s), want both", c)
	}
	pr, err := links.PairLinks(ctx, key)
	if err != nil || len(pr.Candidates) != 2 {
		t.Fatalf("pair: %v %+v", err, pr)
	}
	if _, err := links.ConfirmLinks(ctx, ns, []string{pr.Candidates[0].ID, pr.Candidates[1].ID}, "operator:ada"); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if _, conf := readable(denied); conf != 1 {
		t.Fatalf("after the pair confirm the non-payroll reader holds %d confirmation(s), want the directory row's", conf)
	}

	rec, err := er.Erase(ctx, erasureReq("lctest-op", domain.ErasureSelector{DocumentIDs: []string{"lctest-dir1"}}))
	if err != nil {
		t.Fatalf("erase: %v", err)
	}
	var left []string
	rows, err := a.pool.Query(ctx, `SELECT source_ref FROM links WHERE namespace_id=$1 ORDER BY source_ref`, ns)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		left = append(left, s)
	}
	rows.Close()
	if len(left) != 2 || left[1] != "lctest:payroll" || !strings.HasPrefix(left[0], "confirm:") {
		t.Fatalf("links left after erasing the directory row = %v, want the payroll row and its confirmation only", left)
	}
	if rec.Counts[eraseLinks] != 2 {
		t.Errorf("erasure receipt counts %v, want the open candidate and its confirmation under %q", rec.Counts, eraseLinks)
	}
	if c, conf := readable(denied); c != 0 || conf != 0 {
		t.Errorf("after erasure the non-payroll reader still reaches the pair (%d candidates, %d confirmations)", c, conf)
	}
	if _, conf := readable(granted); conf != 1 {
		t.Errorf("after erasure the payroll reader must still hold the payroll row's confirmation (got %d)", conf)
	}
}
