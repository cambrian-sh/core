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

// stubSyncProcessor implements SyncProcessor for CHAT routing tests.
type stubSyncProcessor struct {
	calledWith domain.Signal
	response   *domain.Handoff
	err        error
}

func (p *stubSyncProcessor) OnSignal(_ context.Context, _ domain.Signal) error { return nil }

func (p *stubSyncProcessor) ProcessSync(_ context.Context, sig domain.Signal) (*domain.Handoff, error) {
	p.calledWith = sig
	return p.response, p.err
}

// Cycle 1 — Execute CHAT decision with SyncProcessor-capable SignalReceiver calls ProcessSync.
func TestServer_Execute_ChatDecision_CallsProcessSync(t *testing.T) {
	wantHandoff := &domain.Handoff{
		FromAgent: "conversation-daemon",
		Payload:   &domain.Payload{Type: "text", Data: []byte("Hello!")},
	}
	proc := &stubSyncProcessor{response: wantHandoff}

	s := &Server{
		Router:          &stubChatRouter{},
		SignalReceiver:  proc,
	}

	req := &pb.Handoff{
		Payload: &pb.Object{Data: []byte("hi there")},
		Metadata: map[string]string{
			"_conversation_id": "conv-abc",
		},
	}

	resp, err := s.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// The response should come from ProcessSync, not the not_implemented stub.
	if resp.Payload == nil || string(resp.Payload.Data) != "Hello!" {
		t.Errorf("Execute CHAT: want daemon response 'Hello!', got payload=%v", resp.Payload)
	}

	// ProcessSync must have been called with the correct stream ID.
	if proc.calledWith.StreamID != "conv-abc" {
		t.Errorf("ProcessSync StreamID: want %q, got %q", "conv-abc", proc.calledWith.StreamID)
	}
}

// Cycle 2 — Execute CHAT decision with no SyncProcessor still returns not_implemented (graceful).
func TestServer_Execute_ChatDecision_NoSyncProcessor_ReturnsNotImplemented(t *testing.T) {
	// SignalReceiver does not implement SyncProcessor (NoOpSignalReceiver only has OnSignal).
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
		t.Errorf("want not_implemented when no SyncProcessor, got type=%q", resp.Payload.GetType())
	}
}

// Cycle 3 — Execute CHAT with nil SignalReceiver falls back to not_implemented.
func TestServer_Execute_ChatDecision_NilSignalReceiver_ReturnsNotImplemented(t *testing.T) {
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
