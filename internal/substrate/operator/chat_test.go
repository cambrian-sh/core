package operator_test

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/cambrian-runtime/api/proto"
	"github.com/cambrian-sh/cambrian-runtime/internal/substrate/operator"
)

// CreateSession creates exactly one session and is idempotent on command_id.
func TestCreateSession_CreatesAndIsIdempotent(t *testing.T) {
	svc, _, _, _ := newCommandService()
	svc.SetCommandEffects(operator.NoopEffects{})
	creates := 0
	svc.SetSessionOps(operator.SessionOpsFuncs{
		CreateFn: func(_ context.Context, goal, _ string) (string, error) {
			creates++
			return "sess-" + goal, nil
		},
	})

	req := &pb.CreateSessionRequest{CommandId: "cs1", Reason: "start work", Goal: "x"}
	resp, err := svc.CreateSession(opCtx(), req)
	if err != nil || resp.GetSessionId() != "sess-x" {
		t.Fatalf("expected new session sess-x, got %+v err=%v", resp, err)
	}
	resp2, err := svc.CreateSession(opCtx(), req) // retry
	if err != nil || !resp2.GetDeduped() {
		t.Fatalf("retry should dedup, got %+v err=%v", resp2, err)
	}
	if creates != 1 {
		t.Fatalf("expected exactly one session creation, got %d", creates)
	}
}

// An unwired dispatch hook returns Unimplemented (honest), not a silent success.
func TestSendMessage_UnwiredIsUnimplemented(t *testing.T) {
	svc, _, _, _ := newCommandService()
	svc.SetCommandEffects(operator.NoopEffects{})
	svc.SetSessionOps(operator.SessionOpsFuncs{}) // no SendFn

	_, err := svc.SendMessage(opCtx(), &pb.SendMessageRequest{
		CommandId: "m1", Reason: "ask", SessionId: "s1", Text: "hello",
	})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("expected Unimplemented for unwired send, got %v", err)
	}
}
