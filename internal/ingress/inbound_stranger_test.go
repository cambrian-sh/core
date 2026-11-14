package ingress

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// guestIdentities: every sender is unbound; the surface's stranger policy is
// guest_principal with the given guest.
type guestIdentities struct{ guest string }

func (guestIdentities) ResolveIdentity(context.Context, string, domain.SenderProfile) (domain.IdentityBinding, bool) {
	return domain.IdentityBinding{}, false
}

func (g guestIdentities) StrangerPolicyFor(context.Context, string) domain.StrangerPolicy {
	return domain.StrangerPolicy{Mode: domain.StrangerGuestPrincipal, GuestPrincipalID: g.guest}
}

// T-20261113-chat-ingress-ignores-the-stranger-guest: under guest_principal an
// unbound sender's turn runs as the configured guest bucket (an agent, as on the
// MCP door) — not under the chat worker's own reach.
func TestAccept_GuestPrincipalStrangerRunsAsTheGuest(t *testing.T) {
	s, _, turns := inboundFixture(t)
	s.SetIdentityResolver(guestIdentities{guest: "chat-guests"})

	if err := s.Accept(context.Background(), InboundMessage{Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:12345", Text: "hello"}); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if len(turns.prins) != 1 || turns.prins[0] != domain.AgentPrincipal("chat-guests") {
		t.Fatalf("stranger turn ran as %+v, want agent:chat-guests", turns.prins)
	}
}

// A guest mode naming no guest refuses, before any conversation exists — it must
// never fall back to the surface default (the opposite of what was configured).
func TestAccept_GuestModeWithoutAGuestRefuses(t *testing.T) {
	s, b, turns := inboundFixture(t)
	s.SetIdentityResolver(guestIdentities{})

	err := s.Accept(context.Background(), InboundMessage{Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:12345", Text: "hello"})
	if !errors.Is(err, ErrSenderNotBound) {
		t.Fatalf("expected ErrSenderNotBound, got %v", err)
	}
	if b.createN != 0 || len(turns.ran) != 0 {
		t.Errorf("a refused sender must leave no trace: %d convs, %d turns", b.createN, len(turns.ran))
	}
}
