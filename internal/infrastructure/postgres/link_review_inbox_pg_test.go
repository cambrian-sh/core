package postgres

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// The link review inbox's membership and order against a real, migrated Postgres
// (PG_TEST_DSN; `orchestrator migrate up`, which applies 0038).
//
// Owner decisions 2026-11-12 (ADR-0128 §3/D4/D9):
//   - a candidate a person confirmed leaves the inbox and the entity card's
//     candidate lane (T-20261112-confirmed-link-candidates-stay-in-the-review);
//     ConfirmLink stays append-only, so membership is decided by the confirmation
//     row, never by editing the candidate;
//   - the inbox order is confidence DESC, asserted_at ASC (oldest first), id —
//     the console's orderCandidates;
//   - the entity card reads ONE subject's candidates in SQL before the limit
//     (T-20261112-entity-op-s-candidates-read-cuts-the-whole).

func inboxCandidate(ev domain.EvidenceID, from, to, source string, conf float64, at time.Time) domain.Link {
	return domain.Link{
		NamespaceID: identityNS, Family: domain.LinkFamilyIdentity,
		FromRef: domain.EntityRef(from), ToRef: domain.EntityRef(to),
		Relation: domain.RelationSameAs, State: domain.LinkStateCandidate,
		Mechanism: domain.LinkMechanismScored, Producer: "inbox@1", Confidence: conf,
		EvidenceID: ev, AssertedBy: "producer:inbox", AssertedAt: at, SourceRef: source,
	}
}

func inboxIDs(ls []domain.Link) []string {
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.SourceRef)
	}
	return out
}

func TestPgLinkStore_ConfirmedCandidateLeavesTheInbox(t *testing.T) {
	_, links, pool, ctx := newIdentityStores(t)
	ev := seedEvidence(t, ctx, pool, "ev-inbox-confirm")
	now := time.Now().UTC()

	for _, l := range []domain.Link{
		inboxCandidate(ev, "cust/a", "cust/b", "s-confirm", 0.9, now),
		inboxCandidate(ev, "cust/c", "cust/d", "s-reject", 0.8, now),
		inboxCandidate(ev, "cust/e", "cust/f", "s-open", 0.7, now),
	} {
		if _, err := links.PutLink(ctx, l); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	before, err := links.Candidates(ctx, identityNS, 10)
	if err != nil || len(before) != 3 {
		t.Fatalf("Candidates before: %v (%v)", err, inboxIDs(before))
	}
	byRef := map[string]domain.Link{}
	for _, l := range before {
		byRef[l.SourceRef] = l
	}

	if _, err := links.ConfirmLink(ctx, identityNS, byRef["s-confirm"].ID, "operator:ada"); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if err := links.RetractLink(ctx, identityNS, byRef["s-reject"].ID, "operator:ada", "wrong"); err != nil {
		t.Fatalf("reject: %v", err)
	}

	after, err := links.Candidates(ctx, identityNS, 10)
	if err != nil {
		t.Fatalf("Candidates after: %v", err)
	}
	if got := inboxIDs(after); len(got) != 1 || got[0] != "s-open" {
		t.Fatalf("inbox after confirm+reject = %v, want [s-open]: a confirmed or rejected candidate is answered", got)
	}

	// The entity lane agrees with the inbox for the confirmed subject.
	lane, err := links.CandidatesForRefs(ctx, identityNS,
		[]string{domain.EntityRef("cust/a"), domain.EntityRef("cust/b"), domain.EntityRef("cust/e")}, 0, 10)
	if err != nil {
		t.Fatalf("CandidatesForRefs: %v", err)
	}
	if n := len(lane[domain.EntityRef("cust/a")]) + len(lane[domain.EntityRef("cust/b")]); n != 0 {
		t.Fatalf("the confirmed candidate is still on its entities' cards (%d rows)", n)
	}
	if got := inboxIDs(lane[domain.EntityRef("cust/e")]); len(got) != 1 || got[0] != "s-open" {
		t.Fatalf("an open candidate vanished from its entity's card: %v", got)
	}

	// Append-only is kept: the candidate row itself is untouched and still readable.
	still, err := links.LinksFor(ctx, identityNS, domain.EntityRef("cust/a"), domain.LinkQuery{State: domain.LinkStateCandidate})
	if err != nil || len(still) != 1 || still[0].ID != byRef["s-confirm"].ID || still[0].RetractedAt != nil {
		t.Fatalf("confirmation altered the candidate row: %v %+v", err, still)
	}

	// Retracting the CONFIRMATION does not send the proposal back for review: the
	// person decided it twice, and re-confirming would only replay the standing row.
	var confID string
	if err := pool.QueryRow(ctx, `SELECT id FROM links WHERE namespace_id=$1 AND source_ref=$2`,
		identityNS, "confirm:"+byRef["s-confirm"].ID).Scan(&confID); err != nil {
		t.Fatalf("confirmation row: %v", err)
	}
	if err := links.RetractLink(ctx, identityNS, confID, "operator:ada", "changed my mind"); err != nil {
		t.Fatalf("retract confirmation: %v", err)
	}
	again, err := links.Candidates(ctx, identityNS, 10)
	if err != nil || len(again) != 1 || again[0].SourceRef != "s-open" {
		t.Fatalf("inbox after retracting the confirmation = %v (%v)", inboxIDs(again), err)
	}
}

func TestPgLinkStore_InboxOrderIsConfidenceThenOldestFirst(t *testing.T) {
	_, links, pool, ctx := newIdentityStores(t)
	ev := seedEvidence(t, ctx, pool, "ev-inbox-order")
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

	// The equal-confidence rows are inserted oldest-first, so recorded_at grows
	// with asserted_at: the old order (recorded_at DESC) would put "newer" before
	// "older", the wanted one (asserted_at ASC) the reverse.
	for _, l := range []domain.Link{
		inboxCandidate(ev, "o/1", "o/2", "low", 0.5, t0),
		inboxCandidate(ev, "o/7", "o/8", "older", 0.8, t0),
		inboxCandidate(ev, "o/5", "o/6", "middle", 0.8, t0.Add(time.Hour)),
		inboxCandidate(ev, "o/3", "o/4", "newer", 0.8, t0.Add(2*time.Hour)),
		inboxCandidate(ev, "o/9", "o/10", "top", 0.95, t0.Add(3*time.Hour)),
	} {
		if _, err := links.PutLink(ctx, l); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	got, err := links.Candidates(ctx, identityNS, 10)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	want := []string{"top", "older", "middle", "newer", "low"}
	if fmt.Sprint(inboxIDs(got)) != fmt.Sprint(want) {
		t.Fatalf("inbox order = %v, want %v (confidence DESC, asserted_at ASC)", inboxIDs(got), want)
	}

	// The per-subject read uses the same order.
	for _, l := range []domain.Link{
		inboxCandidate(ev, "subj/x", "o/a", "x-new", 0.6, t0.Add(time.Hour)),
		inboxCandidate(ev, "o/b", "subj/x", "x-old", 0.6, t0),
		inboxCandidate(ev, "subj/x", "o/c", "x-high", 0.9, t0.Add(5*time.Hour)),
	} {
		if _, err := links.PutLink(ctx, l); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	lane, err := links.CandidatesForRefs(ctx, identityNS, []string{domain.EntityRef("subj/x")}, 0, 10)
	if err != nil {
		t.Fatalf("CandidatesForRefs: %v", err)
	}
	if got, want := inboxIDs(lane[domain.EntityRef("subj/x")]), []string{"x-high", "x-old", "x-new"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("subject lane order = %v, want %v", got, want)
	}

	// The order is served by the rebuilt idx_links_review (0038).
	var def string
	if err := pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE indexname='idx_links_review'`).Scan(&def); err != nil {
		t.Fatalf("idx_links_review: %v", err)
	}
	if want := "(namespace_id, confidence DESC, asserted_at, id)"; !strings.Contains(def, want) {
		t.Fatalf("idx_links_review = %q, want it to cover %s", def, want)
	}
}

func TestPgLinkStore_CandidatesForRefsFiltersBeforeTheLimit(t *testing.T) {
	_, links, pool, ctx := newIdentityStores(t)
	ev := seedEvidence(t, ctx, pool, "ev-inbox-subject")
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	const limit = 3

	// 2*limit higher-confidence candidates that do not touch the subject.
	for i := 0; i < 2*limit; i++ {
		if _, err := links.PutLink(ctx, inboxCandidate(ev, fmt.Sprintf("noise/%d", i),
			fmt.Sprintf("noise/x%d", i), fmt.Sprintf("noise-%d", i), 0.99, t0)); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	// 2*limit+1 candidates touching the subject (either endpoint).
	for i := 0; i < 2*limit+1; i++ {
		l := inboxCandidate(ev, "subj/s", fmt.Sprintf("other/%d", i), fmt.Sprintf("s-%02d", i), 0.5,
			t0.Add(time.Duration(i)*time.Minute))
		if i%2 == 1 {
			l.FromRef, l.ToRef = l.ToRef, l.FromRef
		}
		if _, err := links.PutLink(ctx, l); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	ref := domain.EntityRef("subj/s")
	var all []string
	for off := 0; ; off += limit {
		page, err := links.CandidatesForRefs(ctx, identityNS, []string{ref}, off, limit)
		if err != nil {
			t.Fatalf("page %d: %v", off, err)
		}
		rows := page[ref]
		if off < 2*limit && len(rows) != limit {
			t.Fatalf("page at offset %d has %d rows, want a full page of %d", off, len(rows), limit)
		}
		all = append(all, inboxIDs(rows)...)
		if len(rows) < limit {
			break
		}
	}
	if len(all) != 2*limit+1 {
		t.Fatalf("paged %d subject candidates, want %d: %v", len(all), 2*limit+1, all)
	}
	for i, id := range all {
		if want := fmt.Sprintf("s-%02d", i); id != want {
			t.Fatalf("subject candidate %d = %s, want %s (oldest first among equals)", i, id, want)
		}
	}
}

// The entity card's candidate lane, through the plane: a subject's candidate is
// listed even when the inbox holds more than `limit` higher-confidence candidates
// about other entities, and a confirmed one is not listed again.
func TestPg_EntityOp_CandidateLaneReadsTheSubjectNotTheInbox(t *testing.T) {
	plane, pool, ctx := newScopePlane(t)
	ev := seedClassifiedEvidence(t, ctx, pool, "k2scope-ev-lane", []string{"k2-open"})
	links := NewPgLinkStore(pool, nil)
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	put := func(from, to, src string, conf float64) {
		t.Helper()
		if _, err := links.PutLink(ctx, domain.Link{
			NamespaceID: "default", Family: domain.LinkFamilyIdentity,
			FromRef: domain.EntityRef(from), ToRef: domain.EntityRef(to),
			Relation: domain.RelationSameAs, State: domain.LinkStateCandidate,
			Mechanism: domain.LinkMechanismScored, EvidenceID: ev, Confidence: conf,
			AssertedBy: "test", AssertedAt: t0, Producer: "k2scope-lane@1", SourceRef: src,
		}); err != nil {
			t.Fatalf("put %s: %v", src, err)
		}
	}
	const limit = 4
	for i := 0; i < 2*limit; i++ {
		put(fmt.Sprintf("k2scope/noise%d", i), fmt.Sprintf("k2scope/noisex%d", i), fmt.Sprintf("k2scope-noise-%d", i), 0.99)
	}
	put("k2scope/lane", "k2scope/peer", "k2scope-lane-open", 0.4)
	put("k2scope/peer2", "k2scope/lane", "k2scope-lane-confirmed", 0.3)

	var confirmID string
	if err := pool.QueryRow(ctx, `SELECT id FROM links WHERE source_ref='k2scope-lane-confirmed'`).Scan(&confirmID); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if _, err := links.ConfirmLink(ctx, "default", confirmID, "operator:ada"); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	// ENG-1536: candidates are the operator plane's.
	opCtx := domain.WithSurface(ctx, domain.SurfaceRef{Kind: domain.SurfaceOperator, ID: "console"})
	res, err := plane.Query(opCtx, domain.KnowledgeQuery{Kind: domain.QueryEntity, EntityID: "k2scope/lane", Limit: limit},
		&domain.TagPredicate{Bypass: true})
	if err != nil {
		t.Fatalf("entity op: %v", err)
	}
	var cands []string
	for _, row := range res.Rows {
		if row["row"] == "candidate" {
			cands = append(cands, fmt.Sprint(row["source_ref"]))
		}
	}
	if len(cands) != 1 || cands[0] != "k2scope-lane-open" {
		t.Fatalf("entity card candidates = %v, want [k2scope-lane-open] (subject's open candidate, not the confirmed one, not lost behind the inbox)", cands)
	}
}
