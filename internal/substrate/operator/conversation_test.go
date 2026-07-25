package operator_test

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// fakeConvOps is an in-memory operator.ConversationOps for the handler tests.
type fakeConvOps struct {
	owners   map[string]string // conversation id -> owner
	profiles map[string]string
	sendN    int
	closed   map[string]bool
}

func newFakeConvOps() *fakeConvOps {
	return &fakeConvOps{owners: map[string]string{}, profiles: map[string]string{}, closed: map[string]bool{}}
}

func (f *fakeConvOps) Open(_ context.Context, id, ownerID, _, profile, _ string) (bool, error) {
	if existing, ok := f.owners[id]; ok {
		if existing != ownerID {
			return false, operator.ErrConversationForbidden
		}
		return true, nil
	}
	f.owners[id] = ownerID
	f.profiles[id] = profile
	return false, nil
}

func (f *fakeConvOps) SendTurn(_ context.Context, id, text, clientID string) (domain.Message, error) {
	f.sendN++
	return domain.Message{ID: "reply-" + clientID, ConversationID: id, Seq: 2, Role: domain.MessageRoleAgent, Content: "echo:" + text}, nil
}

func (f *fakeConvOps) Close(_ context.Context, id string) error {
	f.closed[id] = true
	return nil
}

func (f *fakeConvOps) Messages(_ context.Context, id string, afterSeq int64, _ int) ([]domain.Message, error) {
	return []domain.Message{{ID: "m1", ConversationID: id, Seq: 1, Role: domain.MessageRoleUser, Content: "hi"}}, nil
}

func (f *fakeConvOps) Owner(_ context.Context, id string) (string, error) {
	o, ok := f.owners[id]
	if !ok {
		return "", domain.ErrConversationNotFound
	}
	return o, nil
}

func (f *fakeConvOps) List(_ context.Context, ownerID string, _ int) ([]domain.Conversation, error) {
	var out []domain.Conversation
	for id, owner := range f.owners {
		if owner == ownerID {
			out = append(out, domain.Conversation{ID: id, OwnerID: owner, Status: domain.ConversationOpen, Profile: domain.ProfileOperator})
		}
	}
	return out, nil
}

func bobCtx() context.Context {
	return operator.ContextWithPrincipal(context.Background(), "bob", operator.RoleOperator)
}

func convService(t *testing.T) (*operator.Service, *fakeConvOps) {
	t.Helper()
	svc, _, _, _ := newCommandService()
	svc.SetCommandEffects(operator.NoopEffects{})
	ops := newFakeConvOps()
	svc.SetConversationOps(ops)
	return svc, ops
}

// With no adapter wired, the whole surface is honestly Unimplemented.
func TestConversation_UnwiredIsUnimplemented(t *testing.T) {
	svc, _, _, _ := newCommandService()
	_, err := svc.SendTurn(opCtx(), &pb.SendTurnOpRequest{CommandId: "c", Reason: "r", ConversationId: "x", Text: "hi"})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("want Unimplemented, got %v", err)
	}
}

// Open stamps the owner from the principal, defaults the profile to operator, and is
// idempotent on the client-supplied conversation id.
func TestOpenConversation_OwnerFromPrincipalAndIdempotent(t *testing.T) {
	svc, ops := convService(t)
	req := &pb.OpenConversationOpRequest{CommandId: "o1", Reason: "start", ConversationId: "conv-1"}

	resp, err := svc.OpenConversation(opCtx(), req)
	if err != nil {
		t.Fatalf("OpenConversation: %v", err)
	}
	if resp.GetDeduped() || resp.GetConversationId() != "conv-1" {
		t.Fatalf("first open: %+v", resp)
	}
	if ops.owners["conv-1"] != "alice" {
		t.Fatalf("owner should be the principal 'alice', got %q", ops.owners["conv-1"])
	}
	if ops.profiles["conv-1"] != string(domain.ProfileOperator) {
		t.Fatalf("profile should default to operator, got %q", ops.profiles["conv-1"])
	}

	resp2, err := svc.OpenConversation(opCtx(), req) // retry
	if err != nil || !resp2.GetDeduped() {
		t.Fatalf("retry should dedup, got %+v err=%v", resp2, err)
	}
}

// A caller-supplied owner in the body must be ignored — ownership is the principal.
func TestOpenConversation_RejectsBadProfile(t *testing.T) {
	svc, _ := convService(t)
	_, err := svc.OpenConversation(opCtx(), &pb.OpenConversationOpRequest{
		CommandId: "o1", Reason: "start", ConversationId: "c", Profile: "root",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for a bad profile, got %v", err)
	}
}

// SendTurn returns the agent reply and is idempotent via command_id (the adapter maps it to
// the store ClientID, so the handler must simply pass it through and not re-gate).
func TestSendTurn_ReturnsReply(t *testing.T) {
	svc, ops := convService(t)
	if _, err := svc.OpenConversation(opCtx(), &pb.OpenConversationOpRequest{CommandId: "o", Reason: "s", ConversationId: "c"}); err != nil {
		t.Fatalf("open: %v", err)
	}
	resp, err := svc.SendTurn(opCtx(), &pb.SendTurnOpRequest{CommandId: "t1", Reason: "ask", ConversationId: "c", Text: "hello"})
	if err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	if resp.GetReply().GetContent() != "echo:hello" || resp.GetReply().GetRole() != "agent" {
		t.Fatalf("unexpected reply: %+v", resp.GetReply())
	}
	if ops.sendN != 1 {
		t.Fatalf("expected 1 dispatch, got %d", ops.sendN)
	}
}

// The ownership boundary: a second operator cannot send into, read, or close a conversation
// they do not own — and the error is PermissionDenied, never a leak of its contents.
func TestConversation_OwnershipEnforced(t *testing.T) {
	svc, _ := convService(t)
	if _, err := svc.OpenConversation(opCtx(), &pb.OpenConversationOpRequest{CommandId: "o", Reason: "s", ConversationId: "c"}); err != nil {
		t.Fatalf("open by alice: %v", err)
	}

	// bob tries to use alice's conversation.
	if _, err := svc.SendTurn(bobCtx(), &pb.SendTurnOpRequest{CommandId: "t", Reason: "r", ConversationId: "c", Text: "x"}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("SendTurn by non-owner: want PermissionDenied, got %v", err)
	}
	if _, err := svc.ListConversationMessages(bobCtx(), &pb.ListConversationMessagesOpRequest{ConversationId: "c"}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("List by non-owner: want PermissionDenied, got %v", err)
	}
	if _, err := svc.CloseConversation(bobCtx(), &pb.CloseConversationOpRequest{CommandId: "cl", Reason: "r", ConversationId: "c"}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("Close by non-owner: want PermissionDenied, got %v", err)
	}

	// bob opening the SAME id also fails (it exists, owned by alice).
	if _, err := svc.OpenConversation(bobCtx(), &pb.OpenConversationOpRequest{CommandId: "o2", Reason: "s", ConversationId: "c"}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("Open of another's id: want PermissionDenied, got %v", err)
	}
}

// A read/send against a nonexistent conversation is NotFound.
func TestConversation_MissingIsNotFound(t *testing.T) {
	svc, _ := convService(t)
	if _, err := svc.ListConversationMessages(opCtx(), &pb.ListConversationMessagesOpRequest{ConversationId: "ghost"}); status.Code(err) != codes.NotFound {
		t.Fatalf("want NotFound, got %v", err)
	}
}

func TestSendTurn_RequiresFields(t *testing.T) {
	svc, _ := convService(t)
	if _, err := svc.SendTurn(opCtx(), &pb.SendTurnOpRequest{CommandId: "t", Reason: "r", ConversationId: "c"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty text: want InvalidArgument, got %v", err)
	}
}
