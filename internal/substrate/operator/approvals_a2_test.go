package operator_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"

	pb "github.com/cambrian-sh/cambrian-runtime/api/proto"
	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/substrate/operator"
)

// chanApprovalHub is an ApprovalHub whose Watch() yields on a channel and whose
// Submit() records the resolved ids — so a test can drive the stream and assert
// the resolve path hits the same id-space.
type chanApprovalHub struct {
	ch      chan domain.ApprovalRequest
	submits []string
}

func (h *chanApprovalHub) Request(context.Context, domain.ApprovalRequest) (domain.ApprovalDecision, error) {
	return domain.ApprovalDecision{}, nil
}
func (h *chanApprovalHub) Watch() (<-chan domain.ApprovalRequest, func()) { return h.ch, func() {} }
func (h *chanApprovalHub) Submit(id string, _ bool, _ string) bool {
	h.submits = append(h.submits, id)
	return true
}

type fakeApprovalStream struct {
	grpc.ServerStream
	ctx  context.Context
	recv chan *pb.ApprovalOp
}

func (f *fakeApprovalStream) Send(a *pb.ApprovalOp) error { f.recv <- a; return nil }
func (f *fakeApprovalStream) Context() context.Context    { return f.ctx }

// WatchToolApprovals streams the ADR-0039 dangerous-tool approval shape
// (tool_name / agent_id / args_preview), and ResolveHITL resolves it over the
// SAME ApprovalHub id-space — the intervention_id IS the approval request id (A2.5).
func TestWatchToolApprovals_StreamsAndSharesApprovalHubIDSpace(t *testing.T) {
	hub := &chanApprovalHub{ch: make(chan domain.ApprovalRequest, 1)}
	feed := operator.NewSpool(operator.SpoolConfig{})
	svc := operator.NewService(feed)
	svc.SetCommandSources(operator.NewInMemoryAuditStore(), domain.NewInMemoryGrantsStore())
	svc.SetSteeringSources(operator.NewExecutionControlHub(), hub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &fakeApprovalStream{ctx: ctx, recv: make(chan *pb.ApprovalOp, 1)}

	go func() { _ = svc.WatchToolApprovals(&pb.SubscribeRequest{}, stream) }()

	hub.ch <- domain.ApprovalRequest{ID: "appr-1", AgentID: "agent-9", ToolName: "shell_exec", ArgsPreview: `{"cmd":"rm -rf"}`}

	select {
	case got := <-stream.recv:
		if got.GetId() != "appr-1" || got.GetToolName() != "shell_exec" || got.GetArgsPreview() != `{"cmd":"rm -rf"}` || !got.GetIsDestructive() {
			t.Fatalf("ApprovalOp mapping wrong: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an ApprovalOp")
	}

	// Resolve it via the operator plane — hits the SAME hub id.
	if _, err := svc.ResolveHITL(opCtx(), &pb.ResolveHITLRequest{
		CommandId: "r1", Reason: "operator denies", InterventionId: "appr-1", Approve: false,
	}); err != nil {
		t.Fatalf("ResolveHITL: %v", err)
	}
	if len(hub.submits) != 1 || hub.submits[0] != "appr-1" {
		t.Fatalf("ResolveHITL should Submit appr-1 to the hub, got %v", hub.submits)
	}
}

func TestWatchToolApprovals_Unconfigured(t *testing.T) {
	feed := operator.NewSpool(operator.SpoolConfig{})
	svc := operator.NewService(feed)
	stream := &fakeApprovalStream{ctx: context.Background(), recv: make(chan *pb.ApprovalOp, 1)}
	if err := svc.WatchToolApprovals(&pb.SubscribeRequest{}, stream); err == nil {
		t.Fatal("expected Unimplemented when the approval hub is not wired")
	}
}
