package kernel

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/ingress"
	"github.com/cambrian-sh/core/internal/storage"
)

// End to end through the real path (ADR-0090 D8.1, ENG-1172): the production
// DeliveryService over the bbolt-backed journal the composition root wires.

type e2eConvs map[string]domain.Conversation

func (f e2eConvs) GetConversation(_ context.Context, id string) (*domain.Conversation, error) {
	c, ok := f[id]
	if !ok {
		return nil, domain.ErrConversationNotFound
	}
	return &c, nil
}

type e2eTransport struct {
	mu   sync.Mutex
	errs []error // consumed one per call; nil when exhausted
	sent []domain.IngressDelivery
}

func (t *e2eTransport) Deliver(_ context.Context, d domain.IngressDelivery) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.errs) > 0 {
		err := t.errs[0]
		t.errs = t.errs[1:]
		if err != nil {
			return err
		}
	}
	t.sent = append(t.sent, d)
	return nil
}

func (t *e2eTransport) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.sent)
}

func openDeliveryJournal(t *testing.T, path string) (*AgentRepoDecorator, func()) {
	t.Helper()
	st, err := storage.NewBBoltAdapterNoScan(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return NewAgentRepoDecorator(st), func() { _ = st.Close() }
}

func TestDeliveryJournal_TwoPhaseEndToEnd(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agents.db")
	j, closeStore := openDeliveryJournal(t, path)
	convs := e2eConvs{"c1": {ID: "c1", Delivery: domain.DeliveryAddress{IngressAgentID: "tg", ExternalID: "tg:1"}}}

	// 1) A transient failure (no live daemon), then the caller's retry: sent once.
	tr := &e2eTransport{errs: []error{domain.TransientDelivery(errors.New("no daemon"))}}
	svc := ingress.NewDeliveryService(convs, nil, tr)
	svc.SetJournal(j)
	if err := svc.Deliver(ctx, "c1", "hello", "m1"); !errors.Is(err, domain.ErrDeliveryTransient) {
		t.Fatalf("first attempt: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := svc.Deliver(ctx, "c1", "hello", "m1"); err != nil {
			t.Fatalf("retry %d: %v", i, err)
		}
	}
	if tr.count() != 1 {
		t.Fatalf("m1 sent %d times, want 1", tr.count())
	}

	// 2) A crash after prepare: the intent is on disk with its text; after a
	// restart the redriver sends it.
	if _, _, err := j.PrepareDelivery(ctx, domain.DeliveryIntent{
		Key: "delivery:c1:m2", ConversationID: "c1", TxnID: "m2",
		Address: convs["c1"].Delivery, Text: "the answer",
		CreatedAt: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	closeStore() // "crash"
	j, closeStore = openDeliveryJournal(t, path)
	defer closeStore()

	tr2 := &e2eTransport{}
	svc2 := ingress.NewDeliveryService(convs, nil, tr2)
	svc2.SetJournal(j)
	svc2.Redrive(ctx)
	if tr2.count() != 1 || tr2.sent[0].Text != "the answer" {
		t.Fatalf("redrive after restart sent %+v", tr2.sent)
	}
	// m1 was confirmed before the restart: a late retry still does not resend.
	if err := svc2.Deliver(ctx, "c1", "hello", "m1"); err != nil || tr2.count() != 1 {
		t.Fatalf("late retry of a confirmed message: err=%v sent=%d", err, tr2.count())
	}
	states, err := j.ListDeliveries(ctx, []domain.DeliveryIntentState{domain.DeliveryConfirmed}, 0)
	if err != nil || len(states) != 2 {
		t.Fatalf("confirmed intents = %+v, %v; want m1 and m2", states, err)
	}

	// The decorator refuses an illegal transition before touching storage.
	if err := j.AdvanceDelivery(ctx, "delivery:c1:m1", domain.DeliveryConfirmed, domain.DeliveryPrepared, ""); err == nil {
		t.Error("confirmed → prepared must be refused")
	}
}
