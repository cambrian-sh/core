package network

import (
	"context"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/reactive"
)

// stubChatRouter always returns DecisionChat.
type stubChatRouter struct{}

func (r *stubChatRouter) Resolve(_ context.Context, _ domain.RouterInput) (*domain.RouterDecision, error) {
	return &domain.RouterDecision{Type: domain.DecisionChat}, nil
}

// A CHAT decision is never handed to a signal receiver: conversations are
// answered by the chat lane (ADR-0155 D9 / ENG-1500 retired the ProcessSync door).
// Even a receiver that still exposes a ProcessSync method is not called.
type legacySyncReceiver struct{ calls int }

func (p *legacySyncReceiver) OnSignal(_ context.Context, _ domain.Signal) error { return nil }

func (p *legacySyncReceiver) ProcessSync(_ context.Context, _ domain.Signal) (*domain.Handoff, error) {
	p.calls++
	return &domain.Handoff{Payload: &domain.Payload{Type: "text", Data: []byte("Hello!")}}, nil
}

func TestServer_Execute_ChatDecision_NeverReachesASyncDoor(t *testing.T) {
	t.Parallel()
	proc := &legacySyncReceiver{}
	s := &Server{Router: &stubChatRouter{}, SignalReceiver: proc}
	resp, err := s.Execute(context.Background(), &pb.Handoff{
		Payload:  &pb.Object{Data: []byte("hi there")},
		Metadata: map[string]string{"_conversation_id": "conv-abc"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if proc.calls != 0 || resp.GetPayload().GetType() != "not_implemented" {
		t.Fatalf("CHAT reached a sync door (calls=%d) or answered %q, want not_implemented",
			proc.calls, resp.GetPayload().GetType())
	}
}

// Execute CHAT with the OSS receiver returns not_implemented (graceful).
func TestServer_Execute_ChatDecision_OSSReceiver_ReturnsNotImplemented(t *testing.T) {
	t.Parallel()
	noopReceiver := &reactive.NoOpSignalReceiver{}

	s := &Server{
		Router:         &stubChatRouter{},
		SignalReceiver: noopReceiver,
	}

	req := &pb.Handoff{
		Payload:  &pb.Object{Data: []byte("hi")},
		Metadata: map[string]string{"_conversation_id": "conv-xyz"},
	}

	resp, err := s.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.Payload == nil || resp.Payload.Type != "not_implemented" {
		t.Errorf("want not_implemented, got type=%q", resp.Payload.GetType())
	}
}

// Cycle 3 — Execute CHAT with nil SignalReceiver falls back to not_implemented.
func TestServer_Execute_ChatDecision_NilSignalReceiver_ReturnsNotImplemented(t *testing.T) {
	t.Parallel()
	s := &Server{
		Router:         &stubChatRouter{},
		SignalReceiver: nil,
	}

	req := &pb.Handoff{
		Payload:  &pb.Object{Data: []byte("hi")},
		Metadata: map[string]string{"_conversation_id": "conv-nil"},
	}

	resp, err := s.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.Payload == nil || resp.Payload.Type != "not_implemented" {
		t.Errorf("want not_implemented when SignalReceiver is nil, got type=%q", resp.Payload.GetType())
	}
}
