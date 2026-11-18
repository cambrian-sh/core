package app

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// fakeLinkStore records which writes reached the store.
type fakeLinkStore struct {
	domain.LinkStore // unused methods panic
	confirmed        []string
	retracted        []string
}

func (f *fakeLinkStore) ConfirmLink(_ context.Context, _, linkID, actor string) (domain.Link, error) {
	f.confirmed = append(f.confirmed, linkID)
	return domain.Link{ID: "lnk_conf", FromRef: "person:secret", ToRef: "person:other", AssertedBy: actor}, nil
}

func (f *fakeLinkStore) RetractLink(_ context.Context, _, linkID, _, _ string) error {
	f.retracted = append(f.retracted, linkID)
	return nil
}

// fakeLinkReview answers readability per link id and records the scope asked.
type fakeLinkReview struct {
	readable map[string]bool
	scopes   []*domain.TagPredicate
	pairs    []domain.LinkPair
	pairKeys []domain.LinkPairKey
}

func (f *fakeLinkReview) ReviewCandidates(context.Context, string, int, *domain.TagPredicate) ([]domain.Link, error) {
	return nil, nil
}

func (f *fakeLinkReview) LinkReadable(_ context.Context, _, linkID string, scope *domain.TagPredicate) (bool, error) {
	f.scopes = append(f.scopes, scope)
	return f.readable[linkID], nil
}

// T-20261107-operator-confirmlink-retractlink-act-on-and: a link the operator
// plane cannot read (its candidate stands on someone's owner:X evidence) is
// neither confirmed nor retracted, ConfirmLink returns no row for it, and the
// refusal is the SAME as for an unknown id — hidden answers as absent.
func TestLinkWriteSource_RefusesLinksTheOperatorCannotRead(t *testing.T) {
	store := &fakeLinkStore{}
	review := &fakeLinkReview{readable: map[string]bool{"lnk_visible": true}}
	w := linkWriteSource{links: store, review: review}
	ctx := context.Background()

	rec, err := w.ConfirmLink(ctx, "", "lnk_hidden", "operator:ana")
	if !errors.Is(err, domain.ErrLinkRefused) || rec.ID != "" {
		t.Fatalf("confirm hidden: rec=%+v err=%v, want ErrLinkRefused and no row", rec, err)
	}
	_, absentErr := w.ConfirmLink(ctx, "", "lnk_absent", "operator:ana")
	if err.Error() != "link confirm: no link \"lnk_hidden\" in namespace \"default\": "+domain.ErrLinkRefused.Error() ||
		absentErr.Error() != "link confirm: no link \"lnk_absent\" in namespace \"default\": "+domain.ErrLinkRefused.Error() {
		t.Errorf("hidden and absent refusals differ in shape: %q vs %q", err, absentErr)
	}
	if err := w.RetractLink(ctx, "ns1", "lnk_hidden", "operator:ana", "wrong"); !errors.Is(err, domain.ErrLinkRefused) {
		t.Fatalf("retract hidden: err=%v, want ErrLinkRefused", err)
	}
	if len(store.confirmed) != 0 || len(store.retracted) != 0 {
		t.Fatalf("a hidden link reached the store: confirmed=%v retracted=%v", store.confirmed, store.retracted)
	}

	// A readable link still goes through.
	if rec, err := w.ConfirmLink(ctx, "", "lnk_visible", "operator:ana"); err != nil || rec.ID != "lnk_conf" {
		t.Fatalf("confirm visible: rec=%+v err=%v", rec, err)
	}
	if err := w.RetractLink(ctx, "", "lnk_visible", "operator:ana", "wrong"); err != nil {
		t.Fatalf("retract visible: %v", err)
	}
	// Judged at the operator plane's scope: Bypass with the owner fence kept.
	for _, s := range review.scopes {
		if s != domain.ScopeSystem {
			t.Errorf("readability asked at %+v, want ScopeSystem", s)
		}
	}
}

// No review plane: no write (fail closed), like the inbox that lists nothing.
func TestLinkWriteSource_NoReviewPlaneRefusesWrites(t *testing.T) {
	store := &fakeLinkStore{}
	w := linkWriteSource{links: store}
	if _, err := w.ConfirmLink(context.Background(), "", "lnk_x", "operator:ana"); !errors.Is(err, domain.ErrLinkRefused) {
		t.Fatalf("err = %v, want ErrLinkRefused", err)
	}
	if len(store.confirmed) != 0 {
		t.Fatalf("store reached without a review plane")
	}
}

func (f *fakeLinkReview) ReviewPairs(context.Context, string, int, *domain.TagPredicate) ([]domain.LinkPair, error) {
	return f.pairs, nil
}

func (f *fakeLinkReview) ReadablePair(_ context.Context, key domain.LinkPairKey, scope *domain.TagPredicate) (domain.LinkPair, error) {
	f.scopes = append(f.scopes, scope)
	f.pairKeys = append(f.pairKeys, key)
	for _, p := range f.pairs {
		if p.Key == key {
			return p, nil
		}
	}
	return domain.LinkPair{Key: key}, nil
}

// fakePairWriter records the pair-level writes that reached the store.
type fakePairWriter struct {
	confirmed [][]string
	rejected  [][]string
}

func (f *fakePairWriter) ConfirmLinks(_ context.Context, _ string, ids []string, actor string) ([]domain.Link, error) {
	f.confirmed = append(f.confirmed, ids)
	out := make([]domain.Link, 0, len(ids))
	for _, id := range ids {
		out = append(out, domain.Link{ID: "conf_" + id, SourceRef: "confirm:" + id, AssertedBy: actor})
	}
	return out, nil
}

func (f *fakePairWriter) RejectLinkPair(_ context.Context, _ domain.LinkPairKey, ids []string, _ string) (int, error) {
	f.rejected = append(f.rejected, ids)
	return len(ids), nil
}

func pairOf(from, to string, cands, confirmed []string) domain.LinkPair {
	p := domain.LinkPair{Key: domain.LinkPairKey{NamespaceID: "default", Family: domain.LinkFamilyIdentity,
		FromRef: from, ToRef: to, Relation: domain.RelationSameAs}}
	for _, id := range cands {
		p.Candidates = append(p.Candidates, domain.Link{ID: id, Confidence: 0.5, State: domain.LinkStateCandidate,
			SupportingEvidence: []domain.EvidenceID{domain.EvidenceID("ev_" + id)}})
	}
	for _, id := range confirmed {
		p.Confirmed = append(p.Confirmed, domain.Link{ID: id, State: domain.LinkStateConfirmed})
	}
	return p
}

// ENG-2030: the pair inbox maps each claim once with its readable rows, the
// best candidate's confidence, the supporting evidence of each row, and the
// "extend the confirmation?" flag exactly when a readable confirmation exists.
func TestLinkWriteSource_ListLinkPairs(t *testing.T) {
	review := &fakeLinkReview{pairs: []domain.LinkPair{
		pairOf("entity:a/1", "entity:b/2", []string{"c1", "c2"}, nil),
		pairOf("entity:a/3", "entity:b/4", []string{"c3"}, []string{"k1"}),
	}}
	review.pairs[0].Candidates[1].Confidence = 0.9
	w := linkWriteSource{links: &fakeLinkStore{}, review: review, pairs: &fakePairWriter{}}
	got, err := w.ListLinkPairs(context.Background(), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || len(got[0].Candidates) != 2 || got[0].Confidence != 0.9 || got[0].AlreadyConfirmed {
		t.Fatalf("pair 0 = %+v", got)
	}
	if !got[1].AlreadyConfirmed || len(got[1].Confirmations) != 1 {
		t.Fatalf("pair 1 must be flagged already confirmed: %+v", got[1])
	}
	if s := got[0].Candidates[0].SupportingEvidenceIDs; len(s) != 1 || s[0] != "ev_c1" {
		t.Fatalf("supporting evidence not mapped: %v", s)
	}
}

// Q1: confirm on a pair writes one confirmation per READABLE open candidate row,
// read at the operator plane's scope, with the key canonically ordered.
func TestLinkWriteSource_ConfirmLinkPairConfirmsEveryReadableRow(t *testing.T) {
	review := &fakeLinkReview{pairs: []domain.LinkPair{pairOf("entity:a/1", "entity:b/2", []string{"c1", "c2"}, nil)}}
	pw := &fakePairWriter{}
	w := linkWriteSource{links: &fakeLinkStore{}, review: review, pairs: pw}
	// Reversed endpoints name the same identity pair.
	recs, err := w.ConfirmLinkPair(context.Background(), operator.LinkPairKey{
		Family: domain.LinkFamilyIdentity, FromRef: "entity:b/2", ToRef: "entity:a/1", Relation: domain.RelationSameAs,
	}, "operator:ana")
	if err != nil {
		t.Fatal(err)
	}
	if len(pw.confirmed) != 1 || len(pw.confirmed[0]) != 2 || len(recs) != 2 || recs[1].SourceRef != "confirm:c2" {
		t.Fatalf("confirmed=%v recs=%+v", pw.confirmed, recs)
	}
	if review.scopes[0] != domain.ScopeSystem || review.pairKeys[0].FromRef != "entity:a/1" {
		t.Fatalf("read the pair at %v as %+v", review.scopes[0], review.pairKeys[0])
	}
}

// A claim with no readable candidate is refused as an absent one, and nothing
// reaches the store; no pair writer or no review plane fails closed.
func TestLinkWriteSource_PairWritesRefuseUnreadableOrUnwired(t *testing.T) {
	key := operator.LinkPairKey{Family: domain.LinkFamilyIdentity, FromRef: "entity:a/1", ToRef: "entity:b/2", Relation: domain.RelationSameAs}
	for name, w := range map[string]linkWriteSource{
		"hidden":    {links: &fakeLinkStore{}, review: &fakeLinkReview{}, pairs: &fakePairWriter{}},
		"no pairs":  {links: &fakeLinkStore{}, review: &fakeLinkReview{pairs: []domain.LinkPair{pairOf("entity:a/1", "entity:b/2", []string{"c1"}, nil)}}},
		"no review": {links: &fakeLinkStore{}, pairs: &fakePairWriter{}},
	} {
		if _, err := w.ConfirmLinkPair(context.Background(), key, "operator:ana"); !errors.Is(err, domain.ErrLinkRefused) {
			t.Errorf("%s confirm: err = %v, want ErrLinkRefused", name, err)
		}
		if _, err := w.RejectLinkPair(context.Background(), key, "operator:ana", "no"); !errors.Is(err, domain.ErrLinkRefused) {
			t.Errorf("%s reject: err = %v, want ErrLinkRefused", name, err)
		}
		if pw, ok := w.pairs.(*fakePairWriter); ok && (len(pw.confirmed) > 0 || len(pw.rejected) > 0) {
			t.Errorf("%s: the store was reached", name)
		}
	}
}

// Q3: reject on a pair retracts every readable open candidate row.
func TestLinkWriteSource_RejectLinkPairRetractsEveryReadableRow(t *testing.T) {
	review := &fakeLinkReview{pairs: []domain.LinkPair{pairOf("entity:a/1", "entity:b/2", []string{"c1", "c2", "c3"}, nil)}}
	pw := &fakePairWriter{}
	w := linkWriteSource{links: &fakeLinkStore{}, review: review, pairs: pw}
	n, err := w.RejectLinkPair(context.Background(), operator.LinkPairKey{
		Family: domain.LinkFamilyIdentity, FromRef: "entity:a/1", ToRef: "entity:b/2", Relation: domain.RelationSameAs,
	}, "operator:ana", "not the same person")
	if err != nil || n != 3 || len(pw.rejected) != 1 || len(pw.rejected[0]) != 3 {
		t.Fatalf("n=%d err=%v rejected=%v", n, err, pw.rejected)
	}
}
