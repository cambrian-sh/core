package operator_test

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// T-20261106-conversations-cannot-be-opened-as-a-given: an operator opens a
// conversation ON BEHALF OF a principal; the owner is that principal and nothing
// downstream sees the operator.

type fakeDirectory struct {
	known map[string]bool
	err   error
}

func (d fakeDirectory) KnownPrincipal(_ context.Context, id string) (bool, error) {
	return d.known[id], d.err
}

// ctxConvOps wraps fakeConvOps and records the principals the adapter saw.
type ctxConvOps struct {
	*fakeConvOps
	sawDomain, sawRun, sawActedBy string
}

func (c *ctxConvOps) SendTurn(ctx context.Context, id, text, clientID string) (domain.Message, error) {
	c.sawDomain = domain.PrincipalFromContext(ctx).ID
	c.sawRun = domain.RunPrincipalFromContext(ctx)
	c.sawActedBy = domain.ActForActorFromContext(ctx)
	return c.fakeConvOps.SendTurn(ctx, id, text, clientID)
}

func actForService(t *testing.T, dir domain.PrincipalDirectory) (*operator.Service, *ctxConvOps, *operator.InMemoryAuditStore) {
	t.Helper()
	svc, _, audit, _ := newCommandService()
	svc.SetCommandEffects(operator.NoopEffects{})
	ops := &ctxConvOps{fakeConvOps: newFakeConvOps()}
	svc.SetConversationOps(ops)
	if dir != nil {
		svc.SetPrincipalDirectory(dir)
	}
	return svc, ops, audit
}

func TestActFor_OwnerIsThePrincipalAndTurnsRunAsThem(t *testing.T) {
	t.Parallel()
	svc, ops, audit := actForService(t, fakeDirectory{known: map[string]bool{"vantis-sales-dwhitfield": true}})
	ctx := opCtx() // operator "alice"

	if _, err := svc.OpenConversation(ctx, &pb.OpenConversationOpRequest{
		CommandId: "o1", Reason: "suite", ConversationId: "c1", Title: "pipeline",
		OnBehalfOf: "vantis-sales-dwhitfield",
	}); err != nil {
		t.Fatalf("OpenConversation on behalf: %v", err)
	}
	if got := ops.owners["c1"]; got != "vantis-sales-dwhitfield" {
		t.Fatalf("owner = %q, want the principal (never the operator)", got)
	}

	// The operator may not use the conversation as itself: it is the principal's.
	if _, err := svc.SendTurn(ctx, &pb.SendTurnOpRequest{CommandId: "s0", Reason: "r", ConversationId: "c1", Text: "hi"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("send as self into the principal's conversation: want PermissionDenied, got %v", err)
	}
	if _, err := svc.SendTurn(ctx, &pb.SendTurnOpRequest{CommandId: "s1", Reason: "r", ConversationId: "c1", Text: "hi",
		OnBehalfOf: "vantis-sales-dwhitfield"}); err != nil {
		t.Fatalf("send on behalf: %v", err)
	}
	if ops.sawDomain != "vantis-sales-dwhitfield" || ops.sawRun != "vantis-sales-dwhitfield" {
		t.Fatalf("turn ran as domain=%q run=%q, want the principal on both (the operator's identity must not apply)", ops.sawDomain, ops.sawRun)
	}
	// The operator rides along for AUDIT only (T-20261107-a-person-s-assistant-cannot-save-that-person-s):
	// the chat turn's lease carries it so a note write names both.
	if ops.sawActedBy != "alice" {
		t.Fatalf("act-for actor = %q, want the operator (alice)", ops.sawActedBy)
	}

	// Listing on behalf shows the principal's conversation with its owner.
	list, err := svc.ListConversations(ctx, &pb.ListConversationsOpRequest{OnBehalfOf: "vantis-sales-dwhitfield"})
	if err != nil || len(list.GetConversations()) != 1 || list.GetConversations()[0].GetOwnerId() != "vantis-sales-dwhitfield" {
		t.Fatalf("list on behalf: %+v err=%v", list, err)
	}
	// ...and the operator's own list does not include it.
	own, _ := svc.ListConversations(ctx, &pb.ListConversationsOpRequest{})
	if len(own.GetConversations()) != 0 {
		t.Fatalf("the operator's own list must not show the principal's conversation: %+v", own)
	}

	// Audited: the actor is the operator, the entry names the principal.
	entries, _ := audit.Query(context.Background(), operator.AuditFilter{})
	var open, send bool
	for _, e := range entries {
		switch e.ActionType {
		case "open_conversation_on_behalf":
			open = e.Actor == "alice" && e.TargetID == "c1" && e.After == "on_behalf_of=vantis-sales-dwhitfield; title=pipeline"
		case "send_turn_on_behalf":
			send = e.Actor == "alice" && e.After == "on_behalf_of=vantis-sales-dwhitfield"
		}
	}
	if !open || !send {
		t.Fatalf("act-for must be audited with the operator as actor and the principal named; entries=%+v", entries)
	}
}

func TestActFor_UnknownPrincipalRefused(t *testing.T) {
	t.Parallel()
	svc, ops, _ := actForService(t, fakeDirectory{known: map[string]bool{"known": true}})
	_, err := svc.OpenConversation(opCtx(), &pb.OpenConversationOpRequest{
		CommandId: "o1", Reason: "r", ConversationId: "c1", OnBehalfOf: "typo-person",
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("unknown principal: want NotFound, got %v", err)
	}
	if _, ok := ops.owners["c1"]; ok {
		t.Fatal("nothing may be created for an unknown principal")
	}
}

func TestActFor_NoDirectoryFailsClosed(t *testing.T) {
	t.Parallel()
	svc, _, _ := actForService(t, nil)
	_, err := svc.OpenConversation(opCtx(), &pb.OpenConversationOpRequest{
		CommandId: "o1", Reason: "r", ConversationId: "c1", OnBehalfOf: "anyone",
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("no directory: want FailedPrecondition, got %v", err)
	}
	// Acting as oneself still works without a directory.
	if _, err := svc.OpenConversation(opCtx(), &pb.OpenConversationOpRequest{
		CommandId: "o2", Reason: "r", ConversationId: "c2", OnBehalfOf: "alice",
	}); err != nil {
		t.Fatalf("on_behalf_of = self is not act-for: %v", err)
	}
}

func TestActFor_DirectoryErrorFailsClosed(t *testing.T) {
	t.Parallel()
	svc, _, _ := actForService(t, fakeDirectory{err: errors.New("db down")})
	_, err := svc.OpenConversation(opCtx(), &pb.OpenConversationOpRequest{
		CommandId: "o1", Reason: "r", ConversationId: "c1", OnBehalfOf: "p",
	})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("directory error: want Unavailable, got %v", err)
	}
}

func TestActFor_ViewerMayNotActFor(t *testing.T) {
	t.Parallel()
	svc, _, _ := actForService(t, fakeDirectory{known: map[string]bool{"p": true}})
	viewer := operator.ContextWithPrincipal(context.Background(), "val", operator.RoleViewer)
	// ListConversations is a read a Viewer may make for itself — but not for others.
	_, err := svc.ListConversations(viewer, &pb.ListConversationsOpRequest{OnBehalfOf: "p"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("viewer act-for: want PermissionDenied, got %v", err)
	}
	if operator.CanActFor(operator.RoleViewer) || !operator.CanActFor(operator.RoleOperator) {
		t.Fatal("only the Operator role may act for others")
	}
}

// Another operator cannot reach a principal's conversation without naming the
// principal, and naming a DIFFERENT principal is refused by the owner check.
func TestActFor_WrongPrincipalCannotReadTranscript(t *testing.T) {
	t.Parallel()
	svc, _, _ := actForService(t, fakeDirectory{known: map[string]bool{"p1": true, "p2": true}})
	if _, err := svc.OpenConversation(opCtx(), &pb.OpenConversationOpRequest{
		CommandId: "o1", Reason: "r", ConversationId: "c1", OnBehalfOf: "p1",
	}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.ListConversationMessages(opCtx(), &pb.ListConversationMessagesOpRequest{ConversationId: "c1", OnBehalfOf: "p2"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("p2 reading p1's transcript: want PermissionDenied, got %v", err)
	}
	if _, err := svc.ListConversationMessages(opCtx(), &pb.ListConversationMessagesOpRequest{ConversationId: "c1", OnBehalfOf: "p1"}); err != nil {
		t.Fatalf("p1's own transcript: %v", err)
	}
}
