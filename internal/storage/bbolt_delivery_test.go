package storage

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// ADR-0090 D8.1 (ENG-1172): the delivery-intent bucket's prepare-once,
// compare-and-set and prune behaviour, through the production constructor.
func TestDeliveryIntent_PrepareCASListPrune(t *testing.T) {
	path := filepath.Join(t.TempDir(), "delivery.db")
	a, cleanup := openReactiveTestAdapter(t, path)
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)

	rec := DeliveryIntentRecord{Key: "delivery:c1:m1", ConversationID: "c1", TxnID: "m1",
		IngressAgentID: "tg", ExternalID: "tg:1", Text: "hello", State: "prepared", CreatedAt: now, UpdatedAt: now}
	if _, created, err := a.PrepareDeliveryIntent(rec); err != nil || !created {
		t.Fatalf("first prepare: created=%v err=%v", created, err)
	}
	// The same key again is the same message: the stored record wins.
	again := rec
	again.Text = "different"
	got, created, err := a.PrepareDeliveryIntent(again)
	if err != nil || created || got.Text != "hello" {
		t.Fatalf("second prepare = %+v created=%v err=%v; want the stored record", got, created, err)
	}

	if err := a.AdvanceDeliveryIntent(rec.Key, "prepared", "dispatched", "", now, true, false); err != nil {
		t.Fatalf("prepared → dispatched: %v", err)
	}
	// A second dispatcher loses the CAS.
	if err := a.AdvanceDeliveryIntent(rec.Key, "prepared", "dispatched", "", now, true, false); !errors.Is(err, ErrDeliveryIntentConflict) {
		t.Fatalf("second dispatch: want conflict, got %v", err)
	}
	if err := a.AdvanceDeliveryIntent("delivery:nope", "prepared", "dispatched", "", now, true, false); !errors.Is(err, ErrDeliveryIntentNotFound) {
		t.Fatalf("unknown key: want not found, got %v", err)
	}
	if err := a.AdvanceDeliveryIntent(rec.Key, "dispatched", "confirmed", "", now, false, true); err != nil {
		t.Fatalf("dispatched → confirmed: %v", err)
	}

	// Survives a reopen (it is the crash-recovery record).
	cleanup()
	a, cleanup = openReactiveTestAdapter(t, path)
	defer cleanup()
	list, err := a.ListDeliveryIntents([]string{"confirmed"}, 0)
	if err != nil || len(list) != 1 {
		t.Fatalf("list confirmed = %v, %v", list, err)
	}
	if list[0].Attempts != 1 || list[0].Text != "" {
		t.Errorf("confirmed record = %+v; want 1 attempt and the text dropped", list[0])
	}

	// A pending intent is never pruned; an old settled one is.
	pending := DeliveryIntentRecord{Key: "delivery:c1:m2", State: "prepared", Text: "x", CreatedAt: now, UpdatedAt: now}
	if _, _, err := a.PrepareDeliveryIntent(pending); err != nil {
		t.Fatal(err)
	}
	removed, more, err := a.PruneDeliveryIntents(now.Add(time.Hour), 10)
	if err != nil || removed != 1 || more {
		t.Fatalf("prune = %d, %v, %v; want 1 removed", removed, more, err)
	}
	left, _ := a.ListDeliveryIntents([]string{"prepared", "confirmed"}, 0)
	if len(left) != 1 || left[0].Key != pending.Key {
		t.Fatalf("after prune = %+v; want only the pending intent", left)
	}
}
