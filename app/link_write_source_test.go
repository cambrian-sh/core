package app

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
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
