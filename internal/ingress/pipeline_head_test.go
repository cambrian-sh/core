package ingress

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/identity"
)

// ADR-0155 D9 (owner decision 2026-11-14, option a): a conversational ingress
// is a pipeline head. After admission every turn enters a head — a plugin's
// graph head when one handles it, else the kernel's built-in head
// domain.FixedTurnHead — and admission refusals never reach any head.

// recordingHead is a graph head stand-in: it records what it was handed and
// either answers the turn through run (handled) or declines it.
type recordingHead struct {
	handled  bool
	runIt    bool
	err      error
	calls    int
	ingress  []string
	convs    []string
	messages []domain.TurnMessage
}

func (h *recordingHead) RouteTurn(ctx context.Context, ingressAgentID, conversationID string, msg domain.TurnMessage, run domain.TurnFunc) (bool, error) {
	h.calls++
	h.ingress = append(h.ingress, ingressAgentID)
	h.convs = append(h.convs, conversationID)
	h.messages = append(h.messages, msg)
	if h.err != nil {
		return true, h.err
	}
	if h.handled && h.runIt {
		return true, run(ctx, conversationID, msg.Text)
	}
	return h.handled, nil
}

func acceptHello(t *testing.T, s *InboundService) error {
	t.Helper()
	return s.Accept(context.Background(), InboundMessage{
		Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:12345", Text: "hello",
		SpeakerID: "tg:12345", Username: "@ayse", DisplayName: "Ayşe",
	})
}

// No graph head wired (the OSS build): the fixed head runs the one turn with
// the message text — the same call the pre-pipeline path made.
func TestAccept_NoGraphHeadEntersTheFixedHead(t *testing.T) {
	s, b, turns := inboundFixture(t)
	if err := acceptHello(t, s); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if len(turns.ran) != 1 || turns.ran[0].conv != b.convs[0].ID || turns.ran[0].text != "hello" {
		t.Fatalf("fixed head turns = %+v, want one turn (%s, hello)", turns.ran, b.convs[0].ID)
	}
}

// A graph head that answers "not mine" (no armed chat pipeline for this
// ingress) hands the turn to the fixed head: exactly one turn, never zero.
func TestAccept_UnhandledGraphHeadFallsToTheFixedHead(t *testing.T) {
	s, b, turns := inboundFixture(t)
	head := &recordingHead{handled: false}
	s.SetRouter(head)
	if err := acceptHello(t, s); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if head.calls != 1 {
		t.Fatalf("graph head calls = %d, want 1", head.calls)
	}
	if len(turns.ran) != 1 || turns.ran[0].conv != b.convs[0].ID || turns.ran[0].text != "hello" {
		t.Fatalf("turns = %+v, want the fixed head's one turn", turns.ran)
	}
}

// A graph head that handles the turn is the only head: the fixed head does not
// run a second turn behind it.
func TestAccept_HandledGraphHeadIsTheOnlyHead(t *testing.T) {
	s, _, turns := inboundFixture(t)
	head := &recordingHead{handled: true, runIt: true}
	s.SetRouter(head)
	if err := acceptHello(t, s); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if len(turns.ran) != 1 {
		t.Fatalf("turns = %d, want exactly 1 (no fixed-head turn behind a handled graph head)", len(turns.ran))
	}
}

// A graph head's fault is the caller's error; the fixed head does not paper
// over it with a second attempt.
func TestAccept_GraphHeadErrorIsReportedNotRetriedOnTheFixedHead(t *testing.T) {
	s, _, turns := inboundFixture(t)
	boom := errors.New("graph fault")
	s.SetRouter(&recordingHead{err: boom})
	if err := acceptHello(t, s); !errors.Is(err, boom) {
		t.Fatalf("Accept err = %v, want the graph head's fault", err)
	}
	if len(turns.ran) != 0 {
		t.Fatalf("the fixed head ran %d turn(s) behind a failed graph head", len(turns.ran))
	}
}

// The head receives the admitted ingress, conversation and the transported
// sender facts (ADR-0117).
func TestAccept_HeadReceivesTheAdmittedTurn(t *testing.T) {
	s, b, _ := inboundFixture(t)
	head := &recordingHead{handled: false}
	s.SetRouter(head)
	if err := acceptHello(t, s); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	want := domain.TurnMessage{Text: "hello", SenderExternalID: "tg:12345", SpeakerID: "tg:12345", Username: "@ayse", DisplayName: "Ayşe"}
	if head.ingress[0] != tgAgent || head.convs[0] != b.convs[0].ID || head.messages[0] != want {
		t.Fatalf("head got (%s, %s, %+v), want (%s, %s, %+v)", head.ingress[0], head.convs[0], head.messages[0], tgAgent, b.convs[0].ID, want)
	}
}

// Admission stays BEFORE the pipeline: every refusal returns before any head —
// graph or fixed — is entered, so no authored graph can see, reorder or skip a
// security check (ADR-0090 D2).
func TestAccept_AdmissionRefusalsNeverReachAHead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(s *InboundService)
		msg   InboundMessage
	}{
		{"unregistered sender", nil,
			InboundMessage{Sender: domain.AgentPrincipal("not-an-ingress"), ExternalID: "tg:1", Text: "hi"}},
		{"outside the namespace", nil,
			InboundMessage{Sender: domain.AgentPrincipal(tgAgent), ExternalID: "slack:U1", Text: "hi"}},
		{"blocked sender", func(s *InboundService) {
			s.SetIdentityResolver(boundIdentities{bound: true, binding: domain.IdentityBinding{
				Surface: "chat:telegram", ExternalID: "tg:1", BoundToKind: domain.BindPrincipal, BoundToID: "afsin", Blocked: true}})
		}, InboundMessage{Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:1", Text: "hi"}},
		{"stranger refused until bound", func(s *InboundService) {
			s.SetIdentityResolver(boundIdentities{mode: domain.StrangerRefuseUntilBound})
		}, InboundMessage{Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:1", Text: "hi"}},
		{"guest mode with no guest named", func(s *InboundService) {
			s.SetIdentityResolver(boundIdentities{mode: domain.StrangerGuestPrincipal})
		}, InboundMessage{Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:1", Text: "hi"}},
		{"suspended bound person", func(s *InboundService) {
			s.SetIdentityResolver(boundIdentities{bound: true, binding: domain.IdentityBinding{
				Surface: "chat:telegram", ExternalID: "tg:1", BoundToKind: domain.BindPrincipal, BoundToID: "afsin"}})
			reg := identity.NewMemRegistry()
			ctx := context.Background()
			_, _, _ = reg.EnsurePrincipal(ctx, domain.Principal{ID: "afsin"})
			_ = reg.SetStatus(ctx, "afsin", domain.PrincipalSuspended)
			s.SetPrincipals(reg)
		}, InboundMessage{Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:1", Text: "hi"}},
		{"empty text", nil,
			InboundMessage{Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:1", Text: "  "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, b, turns := inboundFixture(t)
			head := &recordingHead{handled: true, runIt: true}
			s.SetRouter(head)
			if tc.setup != nil {
				tc.setup(s)
			}
			if err := s.Accept(context.Background(), tc.msg); err == nil {
				t.Fatal("Accept admitted a message admission must refuse")
			}
			if head.calls != 0 || len(turns.ran) != 0 || b.createN != 0 {
				t.Fatalf("a refused message reached the pipeline: graph head %d, fixed-head turns %d, conversations %d",
					head.calls, len(turns.ran), b.createN)
			}
		})
	}
}
