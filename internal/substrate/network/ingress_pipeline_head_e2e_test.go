package network

import (
	"context"
	"sync"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/ingress"
	ossreactive "github.com/cambrian-sh/core/internal/reactive"
)

// End to end for ADR-0155 D9 stage 5 (ENG-1500), OSS side: a Telegram-style
// message arrives on the agent plane's SignalStream from a registered ingress,
// passes the real admission in ingress.InboundService and enters the kernel's
// fixed pipeline head (domain.FixedTurnHead, ingress → run_turn) — the same
// path app/boot_server.go wires, with the deferred plugin seam answering "not
// mine" as it does in a build without premium. Premium's side of the parity
// (its graph head answers the same turn identically) is
// cambrian-premium/pipeline/chathead_parity_test.go.

type e2eIngresses map[string]domain.IngressRegistration

func (r e2eIngresses) ResolveIngress(_ context.Context, p domain.PrincipalRef) (domain.IngressRegistration, bool) {
	reg, ok := r[p.ID]
	return reg, ok
}

type e2eConversations struct {
	mu    sync.Mutex
	convs []domain.Conversation
}

func (c *e2eConversations) FindByDelivery(_ context.Context, addr domain.DeliveryAddress) (*domain.Conversation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.convs {
		if c.convs[i].Delivery == addr {
			cp := c.convs[i]
			return &cp, nil
		}
	}
	return nil, domain.ErrConversationNotFound
}

func (c *e2eConversations) CreateConversation(_ context.Context, conv domain.Conversation) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.convs = append(c.convs, conv)
	return nil
}

type e2eTurn struct{ conv, text string }

type e2eTurns struct {
	mu  sync.Mutex
	ran []e2eTurn
}

func (t *e2eTurns) RunTurn(_ context.Context, conv, text, _ string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ran = append(t.ran, e2eTurn{conv, text})
	return nil
}

// e2eStrangers resolves nobody and applies one stranger policy.
type e2eStrangers struct{ mode string }

func (e2eStrangers) ResolveIdentity(context.Context, string, domain.SenderProfile) (domain.IdentityBinding, bool) {
	return domain.IdentityBinding{}, false
}

func (s e2eStrangers) StrangerPolicyFor(context.Context, string) domain.StrangerPolicy {
	return domain.StrangerPolicy{Mode: s.mode}
}

// notMineHead is the plugin seam with no graph head registered — what
// app.deferredTurnRouter answers in an OSS build.
type notMineHead struct{ calls int }

func (h *notMineHead) RouteTurn(context.Context, string, string, domain.TurnMessage, domain.TurnFunc) (bool, error) {
	h.calls++
	return false, nil
}

func ingressE2EServer(strangerMode string) (*Server, *e2eConversations, *e2eTurns, *notMineHead) {
	convs, turns, seam := &e2eConversations{}, &e2eTurns{}, &notMineHead{}
	in := ingress.NewInboundService(convs, e2eIngresses{"telegram-bot": {
		AgentID:   "telegram-bot",
		Surface:   domain.SurfaceRef{Kind: domain.SurfaceChat, ID: "telegram"},
		Namespace: []string{"tg:"},
	}}, turns)
	if strangerMode != "" {
		in.SetIdentityResolver(e2eStrangers{mode: strangerMode})
	}
	in.SetRouter(seam)
	return &Server{IngressInbound: in, SignalReceiver: &ossreactive.NoOpSignalReceiver{}}, convs, turns, seam
}

func telegramSignal(ctx context.Context, id, body string) *fakeSignalStream {
	return &fakeSignalStream{ctx: ctx, in: []*pb.Handoff{{
		Id: id, FromAgent: "telegram-bot", Payload: &pb.Object{Data: []byte(body)},
	}}}
}

func TestE2E_TelegramMessageEntersTheFixedChatHead(t *testing.T) {
	t.Parallel()
	s, convs, turns, seam := ingressE2EServer("")
	ctx := domain.WithPrincipal(t.Context(), domain.AgentPrincipal("telegram-bot"))

	_ = s.SignalStream(telegramSignal(ctx, "m1", `{"external_id":"tg:42","text":"where is my order?","speaker_id":42,"speaker_name":"ayse"}`))
	_ = s.SignalStream(telegramSignal(ctx, "m2", `{"external_id":"tg:42","text":"thanks"}`))

	if seam.calls != 2 {
		t.Fatalf("the plugin seam was asked %d times, want 2 (every admitted turn is offered to the graph head first)", seam.calls)
	}
	if len(convs.convs) != 1 {
		t.Fatalf("conversations = %d, want 1 for one sender", len(convs.convs))
	}
	conv := convs.convs[0].ID
	want := []e2eTurn{{conv, "where is my order?"}, {conv, "thanks"}}
	if len(turns.ran) != len(want) || turns.ran[0] != want[0] || turns.ran[1] != want[1] {
		t.Fatalf("fixed head turns = %+v, want %+v", turns.ran, want)
	}
}

// Admission stays before the pipeline on the real path: a refused sender opens
// no conversation and reaches neither the graph seam nor the fixed head.
func TestE2E_RefusedIngressMessageNeverEntersAPipelineHead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, mode, body string }{
		{"outside the namespace", "", `{"external_id":"slack:U1","text":"hi"}`},
		{"stranger refused until bound", domain.StrangerRefuseUntilBound, `{"external_id":"tg:42","text":"hi"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, convs, turns, seam := ingressE2EServer(tc.mode)
			ctx := domain.WithPrincipal(t.Context(), domain.AgentPrincipal("telegram-bot"))
			_ = s.SignalStream(telegramSignal(ctx, "m1", tc.body))
			if seam.calls != 0 || len(turns.ran) != 0 || len(convs.convs) != 0 {
				t.Fatalf("refused message reached the pipeline: seam %d, turns %d, conversations %d",
					seam.calls, len(turns.ran), len(convs.convs))
			}
		})
	}
}
