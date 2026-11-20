package ingress

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ambiguous puts one intent in the ambiguous state the way a real delivery does:
// the send fails with an error that proves nothing.
func ambiguous(t *testing.T, s *DeliveryService, tr *fakeTransport, txn string) string {
	t.Helper()
	tr.err = errors.New("connection reset mid-send")
	if err := s.Deliver(context.Background(), convID, "Your refund is approved.", txn); !errors.Is(err, domain.ErrDeliveryAmbiguous) {
		t.Fatalf("setup: %v, want ambiguous", err)
	}
	tr.err = nil
	return deliveryKey(convID, txn)
}

// ENG-1657 (owner decision 2026-11-20, option A): an operator settles an ambiguous
// intent — confirm (it arrived) or resend once (re-authorised, same txn id) — and
// nothing else: a settled intent is refused, a missing one is not found, a resend
// the far side refuses for good is failed, and an unknown outcome stays ambiguous.
func TestResolveAmbiguous(t *testing.T) {
	ctx := context.Background()
	t.Run("confirm", func(t *testing.T) {
		tr, j := &fakeTransport{}, newFakeJournal()
		s := NewDeliveryService(bound(), registered(), tr)
		s.SetJournal(j)
		key := ambiguous(t, s, tr, "m1")
		if err := s.ResolveAmbiguous(ctx, key, domain.DeliveryResolveConfirm, "customer replied to it"); err != nil {
			t.Fatal(err)
		}
		if tr.count() != 0 || j.intents[key].State != domain.DeliveryConfirmed {
			t.Fatalf("confirm sent %d / state %s", tr.count(), j.intents[key].State)
		}
		if err := s.ResolveAmbiguous(ctx, key, domain.DeliveryResolveResend, "again"); !errors.Is(err, domain.ErrDeliveryIntentNotAmbiguous) {
			t.Fatalf("a settled intent: %v, want ErrDeliveryIntentNotAmbiguous", err)
		}
	})
	t.Run("resend", func(t *testing.T) {
		tr, j := &fakeTransport{}, newFakeJournal()
		s := NewDeliveryService(bound(), registered(), tr)
		s.SetJournal(j)
		key := ambiguous(t, s, tr, "m2")
		if err := s.ResolveAmbiguous(ctx, key, domain.DeliveryResolveResend, "never arrived"); err != nil {
			t.Fatal(err)
		}
		if tr.count() != 1 || tr.sent[0].TxnID != "m2" || tr.sent[0].Text != "Your refund is approved." || j.intents[key].State != domain.DeliveryConfirmed {
			t.Fatalf("resend: sent %+v state %s", tr.sent, j.intents[key].State)
		}
	})
	t.Run("resend refused for good", func(t *testing.T) {
		tr, j := &fakeTransport{}, newFakeJournal()
		s := NewDeliveryService(bound(), registered(), tr)
		s.SetJournal(j)
		key := ambiguous(t, s, tr, "m3")
		tr.err = domain.PermanentDelivery(errors.New("bot was blocked"))
		if err := s.ResolveAmbiguous(ctx, key, domain.DeliveryResolveResend, "r"); !errors.Is(err, domain.ErrDeliveryPermanent) {
			t.Fatalf("permanent: %v", err)
		}
		if j.intents[key].State != domain.DeliveryFailed {
			t.Fatalf("state %s, want failed", j.intents[key].State)
		}
	})
	t.Run("resend still unknown", func(t *testing.T) {
		tr, j := &fakeTransport{}, newFakeJournal()
		s := NewDeliveryService(bound(), registered(), tr)
		s.SetJournal(j)
		key := ambiguous(t, s, tr, "m4")
		tr.err = errors.New("timeout again")
		if err := s.ResolveAmbiguous(ctx, key, domain.DeliveryResolveResend, "r"); !errors.Is(err, domain.ErrDeliveryAmbiguous) {
			t.Fatalf("unknown: %v", err)
		}
		if j.intents[key].State != domain.DeliveryAmbiguous {
			t.Fatalf("state %s, want still ambiguous", j.intents[key].State)
		}
	})
	t.Run("revoked ingress", func(t *testing.T) {
		tr, j := &fakeTransport{}, newFakeJournal()
		s := NewDeliveryService(bound(), registered(), tr)
		s.SetJournal(j)
		key := ambiguous(t, s, tr, "m5")
		s.ingresses = fakeIngresses{} // registration gone since
		if err := s.ResolveAmbiguous(ctx, key, domain.DeliveryResolveResend, "r"); err == nil || tr.count() != 0 {
			t.Fatalf("a revoked ingress was resent to: err=%v sent=%d", err, tr.count())
		}
	})
	t.Run("missing and journal off", func(t *testing.T) {
		s := NewDeliveryService(bound(), registered(), &fakeTransport{})
		if err := s.ResolveAmbiguous(ctx, "x", domain.DeliveryResolveConfirm, "r"); !errors.Is(err, domain.ErrDeliveryJournalOff) {
			t.Fatalf("no journal: %v", err)
		}
		s.SetJournal(newFakeJournal())
		if err := s.ResolveAmbiguous(ctx, "nope", domain.DeliveryResolveConfirm, "r"); !errors.Is(err, domain.ErrDeliveryIntentNotFound) {
			t.Fatalf("missing: %v", err)
		}
	})
}

// ListIntents defaults to the ambiguous and failed intents.
func TestListIntents_DefaultsToAmbiguousAndFailed(t *testing.T) {
	tr, j := &fakeTransport{}, newFakeJournal()
	s := NewDeliveryService(bound(), registered(), tr)
	s.SetJournal(j)
	if err := s.Deliver(context.Background(), convID, "fine", "ok-1"); err != nil {
		t.Fatal(err)
	}
	key := ambiguous(t, s, tr, "amb-1")
	got, err := s.ListIntents(context.Background(), nil, 0)
	if err != nil || len(got) != 1 || got[0].Key != key {
		t.Fatalf("default list = %+v %v, want only the ambiguous intent", got, err)
	}
}
