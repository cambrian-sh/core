package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// Integration tests for the delivery address (ADR-0090). Skips without
// PG_TEST_DSN, and applies the REAL migration so the test cannot drift from the
// shipped DDL.

func openConv(t *testing.T, store *PgConversationStore, ctx context.Context, id string) {
	t.Helper()
	if err := store.CreateConversation(ctx, domain.Conversation{
		ID: id, OwnerID: "alice", Status: domain.ConversationOpen, Profile: domain.ProfileCustomer,
	}); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
}

func TestBindDelivery_RoundTrips(t *testing.T) {
	store, _, ctx := newConvStore(t)
	openConv(t, store, ctx, "conv-deliver-1")

	addr := domain.DeliveryAddress{IngressAgentID: "telegram_ingress", ExternalID: "tg:12345"}
	if err := store.BindDelivery(ctx, "conv-deliver-1", addr); err != nil {
		t.Fatalf("BindDelivery: %v", err)
	}

	got, err := store.GetConversation(ctx, "conv-deliver-1")
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if got.Delivery != addr {
		t.Errorf("delivery address drifted: %+v", got.Delivery)
	}
}

// A conversation that never received anything through an ingress has nowhere to
// deliver — and that is the correct state, not a missing value. It is also
// Telegram's own rule: a bot cannot open a chat with someone who never wrote to it.
func TestBindDelivery_UnboundConversationIsUndeliverable(t *testing.T) {
	store, _, ctx := newConvStore(t)
	openConv(t, store, ctx, "conv-console-only")

	got, err := store.GetConversation(ctx, "conv-console-only")
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if !got.Delivery.IsZero() {
		t.Errorf("a console-only conversation must be undeliverable, got %+v", got.Delivery)
	}
}

// Write-once. Rebinding is a redirect — the single operation an attacker who
// reached this call would want, because it silently retargets every future reply.
func TestBindDelivery_IsWriteOnce(t *testing.T) {
	store, _, ctx := newConvStore(t)
	openConv(t, store, ctx, "conv-deliver-2")

	first := domain.DeliveryAddress{IngressAgentID: "telegram_ingress", ExternalID: "tg:11111"}
	if err := store.BindDelivery(ctx, "conv-deliver-2", first); err != nil {
		t.Fatalf("first bind: %v", err)
	}

	attacker := domain.DeliveryAddress{IngressAgentID: "telegram_ingress", ExternalID: "tg:99999"}
	if err := store.BindDelivery(ctx, "conv-deliver-2", attacker); !errors.Is(err, domain.ErrDeliveryAlreadyBound) {
		t.Fatalf("expected ErrDeliveryAlreadyBound, got %v", err)
	}

	got, _ := store.GetConversation(ctx, "conv-deliver-2")
	if got.Delivery != first {
		t.Errorf("the original recipient must survive a rebind attempt, got %+v", got.Delivery)
	}
}

// The race the SQL guard exists for: two inbound messages arriving at once on
// first contact must not both see "unbound" and let the second redirect the first.
// Exactly one bind wins; every other caller is told it was already bound.
func TestBindDelivery_ConcurrentFirstContact(t *testing.T) {
	store, _, ctx := newConvStore(t)
	openConv(t, store, ctx, "conv-race")

	const n = 12
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		wins    int
		already int
		others  []error
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			addr := domain.DeliveryAddress{IngressAgentID: "telegram_ingress", ExternalID: string(rune('a' + i))}
			err := store.BindDelivery(ctx, "conv-race", addr)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, domain.ErrDeliveryAlreadyBound):
				already++
			default:
				others = append(others, err)
			}
		}(i)
	}
	wg.Wait()

	if len(others) > 0 {
		t.Fatalf("unexpected errors: %v", others)
	}
	if wins != 1 {
		t.Errorf("exactly one bind must win, got %d", wins)
	}
	if already != n-1 {
		t.Errorf("every loser must be told it was already bound, got %d of %d", already, n-1)
	}
}

// "Already bound" and "no such conversation" are different problems for the
// caller, so the store must not collapse them into one vague failure.
func TestBindDelivery_MissingConversation(t *testing.T) {
	store, _, ctx := newConvStore(t)
	addr := domain.DeliveryAddress{IngressAgentID: "telegram_ingress", ExternalID: "tg:1"}
	if err := store.BindDelivery(ctx, "conv-does-not-exist", addr); !errors.Is(err, domain.ErrConversationNotFound) {
		t.Fatalf("expected ErrConversationNotFound, got %v", err)
	}
}

// An address missing either half could never deliver, so it is refused before it
// reaches the database rather than stored as a half-address.
func TestBindDelivery_RefusesIncompleteAddress(t *testing.T) {
	store, _, ctx := newConvStore(t)
	openConv(t, store, ctx, "conv-deliver-3")

	for _, addr := range []domain.DeliveryAddress{
		{IngressAgentID: "telegram_ingress"},
		{ExternalID: "tg:1"},
		{},
	} {
		if err := store.BindDelivery(ctx, "conv-deliver-3", addr); !errors.Is(err, domain.ErrDeliveryAddressInvalid) {
			t.Errorf("addr %+v: expected ErrDeliveryAddressInvalid, got %v", addr, err)
		}
	}
}

// An address supplied at open time is persisted by CreateConversation, so the
// ingress path can bind in one write instead of create-then-update.
func TestCreateConversation_CarriesDeliveryAddress(t *testing.T) {
	store, _, ctx := newConvStore(t)
	addr := domain.DeliveryAddress{IngressAgentID: "telegram_ingress", ExternalID: "tg:777"}
	if err := store.CreateConversation(ctx, domain.Conversation{
		ID: "conv-at-open", OwnerID: "alice", Status: domain.ConversationOpen,
		Profile: domain.ProfileCustomer, Delivery: addr,
	}); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	got, err := store.GetConversation(ctx, "conv-at-open")
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if got.Delivery != addr {
		t.Errorf("address supplied at open did not persist: %+v", got.Delivery)
	}
	// And it counts as bound, so a later bind cannot redirect it.
	if err := store.BindDelivery(ctx, "conv-at-open", domain.DeliveryAddress{
		IngressAgentID: "telegram_ingress", ExternalID: "tg:evil",
	}); !errors.Is(err, domain.ErrDeliveryAlreadyBound) {
		t.Errorf("an address set at open must be write-once too, got %v", err)
	}
}
