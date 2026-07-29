package network

import (
	"context"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"google.golang.org/grpc/metadata"
)

// stubLeaseResolver resolves exactly one lease.
type stubLeaseResolver struct {
	leaseID domain.LeaseID
	binding domain.LeaseBinding
}

// The Server reaches its resolver by type-asserting LLMGateway, so the stub satisfies
// both interfaces. Only ResolveLease is exercised.
func (r *stubLeaseResolver) Acquire(context.Context, domain.StepAllocation, int, time.Duration) (domain.LeaseID, error) {
	return "", nil
}
func (r *stubLeaseResolver) Complete(context.Context, domain.LeaseID) (llm.TokenUsage, error) {
	return llm.TokenUsage{}, nil
}
func (r *stubLeaseResolver) EvictExpired() {}
func (r *stubLeaseResolver) StreamChunks(context.Context, domain.LeaseID, string, domain.GenerateOptions, chan<- domain.StreamChunk) error {
	return nil
}
func (r *stubLeaseResolver) GenerateWithTools(context.Context, domain.LeaseID, []domain.ModelMessage, domain.GenerateOptions, []domain.ToolDefinition) (domain.ModelTurn, error) {
	return domain.ModelTurn{}, nil
}

func (r *stubLeaseResolver) BindLease(domain.LeaseID, domain.LeaseBinding) {}
func (r *stubLeaseResolver) ResolveLease(id domain.LeaseID) (domain.LeaseBinding, bool) {
	if id == r.leaseID {
		return r.binding, true
	}
	return domain.LeaseBinding{}, false
}

// The SDK's delegate() call sends its lease as the handoff metadata field
// `_session_token_id` and sets NO gRPC metadata. Header-only resolution therefore saw a
// caller with no lease, the opened session was never linked to the conversation that
// ordered it, and every session in the store ended up with an empty conversation_id.
func TestResolveBindingFromHandoff_FindsLeaseInThePayload(t *testing.T) {
	res := &stubLeaseResolver{
		leaseID: "lease-abc",
		binding: domain.LeaseBinding{ConversationID: "conv-42", OriginMessageID: "msg-7"},
	}
	s := &Server{LLMGateway: res}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.MD{})

	// No gRPC header — only the handoff metadata, exactly as delegate() sends it.
	b, known := s.resolveBindingFromHandoff(ctx, map[string]string{
		"_session_token_id": "lease-abc",
		"_delegated":        "true",
	})
	if !known {
		t.Fatal("expected the payload-carried lease to resolve")
	}
	if b.ConversationID != "conv-42" {
		t.Errorf("expected conv-42, got %q", b.ConversationID)
	}
}

// The gRPC header stays authoritative: it is set by the transport, the payload by the
// caller, so a caller must not be able to override it with a lease of its choosing.
func TestResolveBindingFromHandoff_HeaderWinsOverPayload(t *testing.T) {
	res := &stubLeaseResolver{
		leaseID: "lease-header",
		binding: domain.LeaseBinding{ConversationID: "conv-from-header"},
	}
	s := &Server{LLMGateway: res}
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("x-lease-id", "lease-header"))

	b, known := s.resolveBindingFromHandoff(ctx, map[string]string{
		"_session_token_id": "lease-someone-elses",
	})
	if !known || b.ConversationID != "conv-from-header" {
		t.Errorf("header must win; got known=%v conv=%q", known, b.ConversationID)
	}
}

func TestResolveBindingFromHandoff_NoLeaseAnywhere(t *testing.T) {
	s := &Server{LLMGateway: &stubLeaseResolver{leaseID: "x"}}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.MD{})

	if _, known := s.resolveBindingFromHandoff(ctx, nil); known {
		t.Error("expected no binding when neither header nor payload carries a lease")
	}
	if _, known := s.resolveBindingFromHandoff(ctx, map[string]string{"_delegated": "true"}); known {
		t.Error("expected no binding for an unknown lease")
	}
}
