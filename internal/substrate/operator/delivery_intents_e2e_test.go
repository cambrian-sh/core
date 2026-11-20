package operator_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/ingress"
	"github.com/cambrian-sh/core/internal/kernel"
	"github.com/cambrian-sh/core/internal/storage"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

type dlvConvs map[string]domain.Conversation

func (f dlvConvs) GetConversation(_ context.Context, id string) (*domain.Conversation, error) {
	c, ok := f[id]
	if !ok {
		return nil, domain.ErrConversationNotFound
	}
	return &c, nil
}

type dlvTransport struct {
	mu   sync.Mutex
	err  error
	sent []domain.IngressDelivery
}

func (t *dlvTransport) Deliver(_ context.Context, d domain.IngressDelivery) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err != nil {
		return t.err
	}
	t.sent = append(t.sent, d)
	return nil
}

// ENG-1657 end to end through the real path: the production DeliveryService over the
// bbolt-backed delivery journal the composition root wires (kernel.AgentRepoDecorator),
// behind the operator RPCs. An ambiguous reply is listed (identifiers, never the text),
// resent once under its txn id and confirmed; another is confirmed by hand; a settled
// intent cannot be resolved again; an unknown key is NotFound; the resolve is audited.
func TestE2E_DeliveryIntentsListedAndResolvedOnTheOperatorPlane(t *testing.T) {
	ctx := opCtx()
	st, err := storage.NewBBoltAdapterNoScan(filepath.Join(t.TempDir(), "agents.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	journal := kernel.NewAgentRepoDecorator(st)
	addr := domain.DeliveryAddress{IngressAgentID: "telegram_ingress", ExternalID: "tg:777"}
	tr := &dlvTransport{}
	dlv := ingress.NewDeliveryService(dlvConvs{"conv-1": {ID: "conv-1", Delivery: addr}}, nil, tr)
	dlv.SetJournal(journal)

	// Two replies whose send ended with an unknown outcome: ambiguous, never resent.
	tr.err = errors.New("connection reset mid-send")
	for _, m := range []string{"m1", "m2"} {
		if err := dlv.Deliver(context.Background(), "conv-1", "Your refund of 742.10 is approved.", m); !errors.Is(err, domain.ErrDeliveryAmbiguous) {
			t.Fatalf("setup %s: %v", m, err)
		}
	}
	tr.err = nil

	svc, _, audit, _ := newCommandService()
	svc.SetDeliveryIntents(dlv)

	list, err := svc.ListDeliveryIntents(ctx, &pb.ListDeliveryIntentsOpRequest{})
	if err != nil || len(list.GetIntents()) != 2 {
		t.Fatalf("list: %+v %v", list, err)
	}
	first := list.GetIntents()[0]
	if first.GetState() != "ambiguous" || first.GetExternalId() != "tg:777" || first.GetTextChars() == 0 {
		t.Fatalf("intent = %+v", first)
	}
	if strings.Contains(first.String(), "refund") {
		t.Fatal("the listing carries the message text")
	}

	// Resend m1: sent once more under its txn id, then confirmed.
	k1 := "delivery:conv-1:m1"
	if _, err := svc.ResolveDeliveryIntent(ctx, &pb.ResolveDeliveryIntentOpRequest{CommandId: "c1", Reason: "never arrived", Key: k1, Action: "resend"}); err != nil {
		t.Fatal(err)
	}
	if len(tr.sent) != 1 || tr.sent[0].TxnID != "m1" || tr.sent[0].Address != addr {
		t.Fatalf("resend: %+v", tr.sent)
	}
	if rows, _ := audit.Query(context.Background(), operator.AuditFilter{CommandID: "c1"}); len(rows) != 1 || rows[0].ActionType != "resolve_delivery_intent" {
		t.Fatalf("audit: %+v", rows)
	}
	// Confirm m2 by hand: nothing sent.
	if _, err := svc.ResolveDeliveryIntent(ctx, &pb.ResolveDeliveryIntentOpRequest{CommandId: "c2", Reason: "customer replied to it", Key: "delivery:conv-1:m2", Action: "confirm"}); err != nil || len(tr.sent) != 1 {
		t.Fatalf("confirm: %v sent=%d", err, len(tr.sent))
	}
	// Nothing left to look at; both are confirmed in the journal.
	if list, _ := svc.ListDeliveryIntents(ctx, &pb.ListDeliveryIntentsOpRequest{}); len(list.GetIntents()) != 0 {
		t.Fatalf("ambiguous/failed after resolving: %+v", list.GetIntents())
	}
	if done, _ := svc.ListDeliveryIntents(ctx, &pb.ListDeliveryIntentsOpRequest{States: []string{"confirmed"}}); len(done.GetIntents()) != 2 {
		t.Fatalf("confirmed = %+v", done.GetIntents())
	}
	// Settled: refused. Unknown: NotFound. Bad action: InvalidArgument.
	if _, err := svc.ResolveDeliveryIntent(ctx, &pb.ResolveDeliveryIntentOpRequest{CommandId: "c3", Reason: "r", Key: k1, Action: "resend"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("settled: %v", err)
	}
	if _, err := svc.ResolveDeliveryIntent(ctx, &pb.ResolveDeliveryIntentOpRequest{CommandId: "c4", Reason: "r", Key: "delivery:conv-1:nope", Action: "confirm"}); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := svc.ResolveDeliveryIntent(ctx, &pb.ResolveDeliveryIntentOpRequest{CommandId: "c5", Reason: "r", Key: k1, Action: "delete"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad action: %v", err)
	}
	if _, err := svc.ListDeliveryIntents(ctx, &pb.ListDeliveryIntentsOpRequest{States: []string{"lost"}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad state: %v", err)
	}
}

// Without a delivery journal (no chat lane) both RPCs answer Unimplemented.
func TestDeliveryIntents_UnimplementedWithoutAJournal(t *testing.T) {
	svc, _, _, _ := newCommandService()
	if _, err := svc.ListDeliveryIntents(opCtx(), &pb.ListDeliveryIntentsOpRequest{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("list: %v", err)
	}
	if _, err := svc.ResolveDeliveryIntent(opCtx(), &pb.ResolveDeliveryIntentOpRequest{CommandId: "c", Reason: "r", Key: "k", Action: "confirm"}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("resolve: %v", err)
	}
}
