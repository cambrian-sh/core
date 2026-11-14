package network

import (
	"context"
	"strings"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// ENG-1029 (owner decision 2026-11-14): Execute stamps the conversation owner of
// a lease presented INSIDE the handoff (_session_token_id — the SDK's delegate),
// exactly as for a header lease; another agent presenting the lease gets nothing,
// and a header lease wins.
func TestWithConversationOwnerFromHandoff(t *testing.T) {
	t.Parallel()
	s, gw := newLeaseServer(t)
	s.ConversationOwner = func(_ context.Context, b domain.LeaseBinding) string {
		return strings.TrimPrefix(b.ConversationID, "conv-of-")
	}
	lease := bindNotesLease(t, gw, domain.LeaseBinding{AgentID: "chat_agent", ConversationID: "conv-of-dana"})
	meta := map[string]string{"_session_token_id": string(lease)}

	if got := domain.ConversationOwnerFromContext(s.withConversationOwnerFromHandoff(agentPlaneCtx("chat_agent"), meta)); got != "dana" {
		t.Fatalf("owner from handoff lease = %q, want dana", got)
	}
	if got := domain.ConversationOwnerFromContext(s.withConversationOwnerFromHandoff(agentPlaneCtx("intruder"), meta)); got != "" {
		t.Fatalf("foreign agent stamped %q", got)
	}
	if got := domain.ConversationOwnerFromContext(s.withConversationOwnerFromHandoff(agentPlaneCtx("chat_agent"), nil)); got != "" {
		t.Fatalf("no lease stamped %q", got)
	}
	other := bindNotesLease(t, gw, domain.LeaseBinding{AgentID: "chat_agent", ConversationID: "conv-of-erol"})
	hdr := agentPlaneCtx("chat_agent", leaseHeader, string(other))
	if got := domain.ConversationOwnerFromContext(s.withConversationOwnerFromHandoff(hdr, meta)); got != "erol" {
		t.Fatalf("header lease must win: owner = %q", got)
	}
}

// ownerRecordingAgent records the conversation owner its dispatch ran under.
type ownerRecordingAgent struct{ owner string }

func (a *ownerRecordingAgent) Execute(context.Context, *domain.DispatchTask, *domain.Handoff) (*domain.DispatchResult, error) {
	return nil, nil
}

func (a *ownerRecordingAgent) CallAgent(ctx context.Context, _ string, _ *domain.Handoff, _ string) (*domain.Handoff, error) {
	a.owner = domain.ConversationOwnerFromContext(ctx)
	return &domain.Handoff{Payload: &domain.Payload{Data: []byte("ok")}}, nil
}

// End to end through Execute: the work done for a handoff-lease caller runs
// with the person's conversation owner on ctx.
func TestExecute_StampsTheConversationOwnerFromTheHandoffLease(t *testing.T) {
	t.Parallel()
	s, gw := newLeaseServer(t)
	s.ConversationOwner = func(_ context.Context, b domain.LeaseBinding) string {
		return strings.TrimPrefix(b.ConversationID, "conv-of-")
	}
	rec := &ownerRecordingAgent{}
	s.Dispatcher = rec
	s.ExecCfg.Lane = config.LaneConfig{Default: config.LaneSingleAgent, SingleAgent: config.SingleAgentLaneConfig{AgentID: "lane_agent"}}
	lease := bindNotesLease(t, gw, domain.LeaseBinding{AgentID: "chat_agent", ConversationID: "conv-of-dana"})
	_, err := s.Execute(agentPlaneCtx("chat_agent"), &pb.Handoff{
		Payload:  &pb.Object{Data: []byte("summarise my open orders")},
		Metadata: map[string]string{"_session_token_id": string(lease)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.owner != "dana" {
		t.Fatalf("Execute ran for owner %q, want dana", rec.owner)
	}
}

// ENG-518 follow-up: the resolved-lease marker distinguishes "no lease
// presented" and "a lease that did not resolve to this caller" from "lease
// resolved" — header or handoff — with or without a conversation-owner resolver.
func TestResolvedLeaseMarker(t *testing.T) {
	t.Parallel()
	s, gw := newLeaseServer(t) // no ConversationOwner resolver wired
	lease := bindNotesLease(t, gw, domain.LeaseBinding{AgentID: "chat_agent"})

	if l, ok := domain.ResolvedLeaseFromContext(s.withConversationOwner(agentPlaneCtx("chat_agent", leaseHeader, string(lease)))); !ok || l != lease {
		t.Fatalf("header lease: marker = %q, %v", l, ok)
	}
	if _, ok := domain.ResolvedLeaseFromContext(s.withConversationOwner(agentPlaneCtx("chat_agent"))); ok {
		t.Fatal("no lease presented, but marked resolved")
	}
	if _, ok := domain.ResolvedLeaseFromContext(s.withConversationOwner(agentPlaneCtx("intruder", leaseHeader, string(lease)))); ok {
		t.Fatal("a foreign agent's presentation was marked resolved")
	}
	if _, ok := domain.ResolvedLeaseFromContext(s.withConversationOwner(agentPlaneCtx("chat_agent", leaseHeader, "garbage"))); ok {
		t.Fatal("an unknown lease was marked resolved")
	}
	meta := map[string]string{"_session_token_id": string(lease)}
	if l, ok := domain.ResolvedLeaseFromContext(s.withConversationOwnerFromHandoff(agentPlaneCtx("chat_agent"), meta)); !ok || l != lease {
		t.Fatalf("handoff lease: marker = %q, %v", l, ok)
	}
}

// ENG-614: the conversation PROFILE is stamped from the same lease lookup — header or
// handoff lease, so a delegated plan inherits it — even for a conversation with no
// owner (an ingress customer), and never for a foreign agent.
func TestStampsConversationProfileFromTheLease(t *testing.T) {
	t.Parallel()
	s, gw := newLeaseServer(t)
	s.ConversationOwner = func(context.Context, domain.LeaseBinding) string { return "" }
	s.ConversationProfile = func(_ context.Context, b domain.LeaseBinding) domain.ConversationProfile {
		if b.ConversationID == "conv-cust" {
			return domain.ProfileCustomer
		}
		return ""
	}
	lease := bindNotesLease(t, gw, domain.LeaseBinding{AgentID: "chat_agent", ConversationID: "conv-cust"})
	meta := map[string]string{"_session_token_id": string(lease)}
	if got := domain.ConversationProfileFromContext(s.withConversationOwnerFromHandoff(agentPlaneCtx("chat_agent"), meta)); got != domain.ProfileCustomer {
		t.Fatalf("handoff lease profile = %q, want customer", got)
	}
	hdr := agentPlaneCtx("chat_agent", leaseHeader, string(lease))
	if got := domain.ConversationProfileFromContext(s.withConversationOwner(hdr)); got != domain.ProfileCustomer {
		t.Fatalf("header lease profile = %q, want customer", got)
	}
	if got := domain.ConversationProfileFromContext(s.withConversationOwnerFromHandoff(agentPlaneCtx("intruder"), meta)); got != "" {
		t.Fatalf("foreign agent stamped profile %q", got)
	}
}
