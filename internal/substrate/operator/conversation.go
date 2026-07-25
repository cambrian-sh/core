package operator

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// ConversationOps is the OSS chat lane seam (ADR-0084 D9): open a conversation, send a
// turn (dispatched to the kernel's stateless worker POOL, not the task planner), read the
// transcript, and close it. The kernel adapter binds these to the ConversationStore and the
// chat TurnService; a nil adapter yields Unimplemented, so an OSS build with the chat lane
// off honestly reports the surface as unavailable.
//
// Ownership is enforced by the SERVICE, not the adapter: every mutating call resolves the
// operator principal from the request context and refuses a conversation owned by anyone
// else. The adapter never sees a caller-supplied owner.
type ConversationOps interface {
	// Open creates the conversation if absent (owner = principal) and reports whether it
	// already existed. An existing conversation owned by a DIFFERENT principal is a
	// permission error.
	Open(ctx context.Context, id, ownerID, title, profile, policy string) (existed bool, err error)
	// SendTurn runs one turn on the pool and returns the persisted agent reply. clientID is
	// the idempotency key: a retry returns the original reply without re-running the turn.
	SendTurn(ctx context.Context, id, text, clientID string) (domain.Message, error)
	// Close marks the conversation closed.
	Close(ctx context.Context, id string) error
	// Messages returns the transcript after a sequence number, ordered.
	Messages(ctx context.Context, id string, afterSeq int64, limit int) ([]domain.Message, error)
	// Owner returns a conversation's owner (for the read-path ownership check), or
	// domain.ErrConversationNotFound.
	Owner(ctx context.Context, id string) (string, error)
	// List returns the conversations owned by ownerID, most recently updated first.
	List(ctx context.Context, ownerID string, limit int) ([]domain.Conversation, error)
}

// ErrConversationForbidden is returned by a ConversationOps adapter when the resolved
// principal does not own the target conversation. The service maps it to PermissionDenied.
var ErrConversationForbidden = errors.New("conversation owned by another principal")

// SetConversationOps wires the OSS chat lane adapter.
func (s *Service) SetConversationOps(ops ConversationOps) { s.convOps = ops }

// HasConversations reports whether the chat lane is wired, so app.go can advertise the
// "chat" capability only when a turn can actually be served.
func (s *Service) HasConversations() bool { return s.convOps != nil }

func (s *Service) requireConvOps() error {
	if s.convOps == nil {
		return status.Error(codes.Unimplemented, "chat lane not configured (execution.chat_pool_size = 0)")
	}
	return nil
}

// mapConvError translates store errors into gRPC status codes so a client can distinguish
// a closed conversation (retryable never) from a missing one (client error).
func mapConvError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrConversationNotFound):
		return status.Error(codes.NotFound, "conversation not found")
	case errors.Is(err, domain.ErrConversationClosed):
		return status.Error(codes.FailedPrecondition, "conversation is closed")
	case errors.Is(err, ErrConversationForbidden):
		return status.Error(codes.PermissionDenied, "conversation is owned by another principal")
	default:
		return err
	}
}

// OpenConversation creates a conversation owned by the resolved operator principal. The
// conversation_id is client-supplied, which makes this idempotent on the id itself: a retry
// returns deduped=true rather than erroring. Audited.
func (s *Service) OpenConversation(ctx context.Context, req *pb.OpenConversationOpRequest) (*pb.OpenConversationOpResponse, error) {
	if err := s.requireConvOps(); err != nil {
		return nil, err
	}
	if req.GetCommandId() == "" || req.GetReason() == "" {
		return nil, status.Error(codes.InvalidArgument, "command_id and reason are required")
	}
	if req.GetConversationId() == "" {
		return nil, status.Error(codes.InvalidArgument, "conversation_id is required")
	}
	profile := req.GetProfile()
	if profile == "" {
		profile = string(domain.ProfileOperator)
	}
	if !domain.ConversationProfile(profile).Valid() {
		return nil, status.Error(codes.InvalidArgument, "profile must be operator, employee, or customer")
	}

	actor, role, _ := PrincipalFromContext(ctx)
	existed, err := s.convOps.Open(ctx, req.GetConversationId(), actor, req.GetTitle(), profile, req.GetPolicy())
	if err != nil {
		return nil, mapConvError(err)
	}

	_, _ = s.recordAndEmit(ctx, domain.AuditEntry{
		ID: newAuditID(), CommandID: req.GetCommandId(), At: time.Now().UTC(),
		Actor: actor, Role: string(role), ActionType: "open_conversation",
		TargetType: "conversation", TargetID: req.GetConversationId(),
		After: req.GetTitle(), Reason: req.GetReason(), Result: "ok",
	})
	return &pb.OpenConversationOpResponse{
		CommandId:      req.GetCommandId(),
		Deduped:        existed,
		ConversationId: req.GetConversationId(),
	}, nil
}

// SendTurn runs one conversational turn on the worker pool and returns the agent's reply.
//
// It deliberately does NOT use runMutation (whose audit record is a dedup gate that returns
// no payload): the turn's idempotency is the store's ClientID replay — command_id is threaded
// as the ClientID, so a retry returns the original reply. The audit entry is recorded
// best-effort after the turn.
func (s *Service) SendTurn(ctx context.Context, req *pb.SendTurnOpRequest) (*pb.SendTurnOpResponse, error) {
	if err := s.requireConvOps(); err != nil {
		return nil, err
	}
	if req.GetCommandId() == "" || req.GetReason() == "" {
		return nil, status.Error(codes.InvalidArgument, "command_id and reason are required")
	}
	if req.GetConversationId() == "" || req.GetText() == "" {
		return nil, status.Error(codes.InvalidArgument, "conversation_id and text are required")
	}
	actor, role, _ := PrincipalFromContext(ctx)
	if err := s.assertOwner(ctx, req.GetConversationId(), actor); err != nil {
		return nil, err
	}

	reply, err := s.convOps.SendTurn(ctx, req.GetConversationId(), req.GetText(), req.GetCommandId())
	if err != nil {
		return nil, mapConvError(err)
	}

	_, _ = s.recordAndEmit(ctx, domain.AuditEntry{
		ID: newAuditID(), CommandID: req.GetCommandId(), At: time.Now().UTC(),
		Actor: actor, Role: string(role), ActionType: "send_turn",
		TargetType: "conversation", TargetID: req.GetConversationId(),
		Reason: req.GetReason(), Result: "ok",
	})
	return &pb.SendTurnOpResponse{CommandId: req.GetCommandId(), Reply: messageToProto(reply)}, nil
}

// CloseConversation closes a conversation the principal owns. Audited + idempotent via the
// audit dedup gate.
func (s *Service) CloseConversation(ctx context.Context, req *pb.CloseConversationOpRequest) (*pb.CommandAck, error) {
	if err := s.requireConvOps(); err != nil {
		return nil, err
	}
	actor, _, _ := PrincipalFromContext(ctx)
	if err := s.assertOwner(ctx, req.GetConversationId(), actor); err != nil {
		return nil, err
	}
	return s.runMutation(ctx, req.GetCommandId(), req.GetReason(), "close_conversation", "conversation",
		req.GetConversationId(), "", func() error { return mapConvError(s.convOps.Close(ctx, req.GetConversationId())) })
}

// ListConversationMessages returns a conversation's transcript. Read RPC (no command_id),
// but still ownership-gated: an operator reads only their own conversations.
func (s *Service) ListConversationMessages(ctx context.Context, req *pb.ListConversationMessagesOpRequest) (*pb.ListConversationMessagesOpResponse, error) {
	if err := s.requireConvOps(); err != nil {
		return nil, err
	}
	actor, _, _ := PrincipalFromContext(ctx)
	if err := s.assertOwner(ctx, req.GetConversationId(), actor); err != nil {
		return nil, err
	}
	msgs, err := s.convOps.Messages(ctx, req.GetConversationId(), req.GetAfterSeq(), int(req.GetLimit()))
	if err != nil {
		return nil, mapConvError(err)
	}
	out := make([]*pb.MessageOp, len(msgs))
	for i, m := range msgs {
		out[i] = messageToProto(m)
	}
	return &pb.ListConversationMessagesOpResponse{Messages: out}, nil
}

// ListConversations returns the calling operator's own conversations (ownership scoped to
// the resolved principal — never a caller-supplied owner). Read RPC, no command_id.
func (s *Service) ListConversations(ctx context.Context, req *pb.ListConversationsOpRequest) (*pb.ListConversationsOpResponse, error) {
	if err := s.requireConvOps(); err != nil {
		return nil, err
	}
	actor, _, _ := PrincipalFromContext(ctx)
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = 100
	}
	convs, err := s.convOps.List(ctx, actor, limit)
	if err != nil {
		return nil, mapConvError(err)
	}
	out := make([]*pb.ConversationOp, len(convs))
	for i, c := range convs {
		out[i] = &pb.ConversationOp{
			Id:        c.ID,
			Title:     c.Title,
			Status:    string(c.Status),
			Profile:   string(c.Profile),
			UpdatedAt: c.UpdatedAt.UTC().Format(time.RFC3339),
		}
	}
	return &pb.ListConversationsOpResponse{Conversations: out}, nil
}

// assertOwner enforces that the principal owns the conversation. A missing conversation is
// reported as NotFound; a mismatch as PermissionDenied — never leaking whether a
// conversation the caller does not own exists.
func (s *Service) assertOwner(ctx context.Context, id, principal string) error {
	owner, err := s.convOps.Owner(ctx, id)
	if err != nil {
		return mapConvError(err)
	}
	if owner != principal {
		return status.Error(codes.PermissionDenied, "conversation is owned by another principal")
	}
	return nil
}

func messageToProto(m domain.Message) *pb.MessageOp {
	return &pb.MessageOp{
		Id:             m.ID,
		ConversationId: m.ConversationID,
		Seq:            m.Seq,
		Role:           string(m.Role),
		Content:        m.Content,
		CreatedAt:      m.CreatedAt.UTC().Format(time.RFC3339),
	}
}
