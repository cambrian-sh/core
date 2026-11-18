package postgres

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ENG-2050 (owner decision 2026-11-18, option b) against a real, migrated
// Postgres: a live row whose source_ref is domain.LinkAnswersSourceRef(<id>)
// answers candidate <id> — whatever lane wrote it, whatever its mechanism or
// state — so the inbox (Candidates, CandidatesForRefs, CandidatePairs) no longer
// lists it and the pair is not offered as "already confirmed; extend?" for the
// candidate that was itself finished. A genuinely new source (a candidate no
// marker names) is still open, beside the answering confirmation. Retracting
// the answer reopens the candidate.
func TestPgLinkStore_AnAnswersMarkerAnswersTheCandidate(t *testing.T) {
	_, links, pool, ctx := newIdentityStores(t)
	ev := seedEvidence(t, ctx, pool, "ev-answers")
	deferred := pairRow(ev, "s-deferred", 0.9, nil)
	deferred.Mechanism = domain.LinkMechanismDeclared
	if _, err := links.PutLink(ctx, deferred); err != nil {
		t.Fatalf("put deferred: %v", err)
	}
	pr, err := links.PairLinks(ctx, pairKeyEP())
	if err != nil || len(pr.Candidates) != 1 {
		t.Fatalf("pair before: %+v err=%v", pr, err)
	}
	candID := pr.Candidates[0].ID

	inbox := func() (rows []string, pairs []domain.LinkPair, forRef []string) {
		t.Helper()
		all, err := links.Candidates(ctx, identityNS, 100)
		if err != nil {
			t.Fatal(err)
		}
		ps, err := links.CandidatePairs(ctx, identityNS, 100)
		if err != nil {
			t.Fatal(err)
		}
		by, err := links.CandidatesForRefs(ctx, identityNS, []string{domain.EntityRef("person/P")}, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		return inboxIDs(all), ps, inboxIDs(by[domain.EntityRef("person/P")])
	}

	// A lane finishes it: a confirmed row carrying the marker (a resolver's).
	answer := pairRow(ev, domain.LinkAnswersSourceRef(candID), 1.0, nil)
	answer.Mechanism, answer.State, answer.Producer = domain.LinkMechanismDeclared, domain.LinkStateConfirmed, "answers-test@1"
	if _, err := links.PutLink(ctx, answer); err != nil {
		t.Fatalf("put answer: %v", err)
	}
	rows, pairs, forRef := inbox()
	if len(rows) != 0 || len(pairs) != 0 || len(forRef) != 0 {
		t.Fatalf("an answered candidate is still in the inbox: rows=%v pairs=%d forRef=%v", rows, len(pairs), forRef)
	}
	pr, err = links.PairLinks(ctx, pairKeyEP())
	if err != nil || len(pr.Candidates) != 0 || len(pr.Confirmed) != 1 {
		t.Fatalf("the pair must read as confirmed by its answer: %+v err=%v", pr, err)
	}

	// A marker naming ANOTHER id answers nothing here; a new source is offered
	// beside the answer as "extend?" (Confirmed non-empty).
	other := pairRow(ev, "s-new-source", 0.7, nil)
	if _, err := links.PutLink(ctx, other); err != nil {
		t.Fatalf("put new source: %v", err)
	}
	stray := inboxCandidate(ev, "person/X1", "person/X2", domain.LinkAnswersSourceRef("lnk_nobody"), 0.5, time.Now().UTC())
	stray.State, stray.Mechanism = domain.LinkStateConfirmed, domain.LinkMechanismDeclared
	if _, err := links.PutLink(ctx, stray); err != nil {
		t.Fatalf("put stray: %v", err)
	}
	rows, pairs, _ = inbox()
	if !slices.Equal(rows, []string{"s-new-source"}) || len(pairs) != 1 || len(pairs[0].Confirmed) != 1 ||
		!slices.Equal(sourceRefs(pairs[0].Candidates), []string{"s-new-source"}) {
		t.Fatalf("a new source must be the pair's only open row, beside the answer: rows=%v pairs=%+v", rows, pairs)
	}

	// Retracting the answer reopens the candidate (a live marker only).
	if err := links.RetractLink(ctx, identityNS, pr.Confirmed[0].ID, "operator:ada", "wrong target"); err != nil {
		t.Fatalf("retract answer: %v", err)
	}
	rows, _, _ = inbox()
	if !slices.Equal(rows, []string{"s-deferred", "s-new-source"}) && !slices.Equal(rows, []string{"s-new-source", "s-deferred"}) {
		t.Fatalf("with its answer retracted the candidate must be open again: %v", rows)
	}
}

// The probe the inbox repeats is the partial index's predicate, so the planner
// can use idx_links_answers (internal/migrate concurrentIndexes).
func TestOpenCandidate_AnswersProbeNamesTheMarker(t *testing.T) {
	want := "source_ref LIKE '" + domain.LinkAnswersSourceRefPrefix + "%' AND an.retracted_at IS NULL"
	if !strings.Contains(openCandidate, "an."+want) {
		t.Fatalf("openCandidate must repeat the index predicate %q:\n%s", want, openCandidate)
	}
	if !strings.Contains(openCandidate, "an.source_ref = '"+domain.LinkAnswersSourceRefPrefix+"' || l.id") {
		t.Fatalf("openCandidate must match the marker of the candidate's own id:\n%s", openCandidate)
	}
}
