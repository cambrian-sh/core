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

// fakeControls records pause/resume/inject calls.
type fakeControls struct {
	paused, resumed int
	injected        []string
}

func (f *fakeControls) Pause()  { f.paused++ }
func (f *fakeControls) Resume() { f.resumed++ }
func (f *fakeControls) Inject(instruction string) error {
	f.injected = append(f.injected, instruction)
	return nil
}

// fakeApprovalHub records Submit calls.
type fakeApprovalHub struct{ submits []string }

func (f *fakeApprovalHub) Request(context.Context, domain.ApprovalRequest) (domain.ApprovalDecision, error) {
	return domain.ApprovalDecision{}, nil
}
func (f *fakeApprovalHub) Watch() (<-chan domain.ApprovalRequest, func()) { return nil, func() {} }
func (f *fakeApprovalHub) Submit(id string, approved bool, _ string) bool {
	f.submits = append(f.submits, id)
	return true
}

func TestExecutionControlHub_RegisterLookupDeregister(t *testing.T) {
	hub := operator.NewExecutionControlHub()
	c := &fakeControls{}
	if _, ok := hub.Lookup("s1"); ok {
		t.Fatal("empty hub should not find s1")
	}
	hub.Register("s1", c)
	got, ok := hub.Lookup("s1")
	if !ok || got != c {
		t.Fatal("expected to find registered controls for s1")
	}
	hub.Deregister("s1")
	if _, ok := hub.Lookup("s1"); ok {
		t.Fatal("deregistered session should not be found")
	}
}

// PauseSession steers the live execution; a retried command is idempotent.
func TestPauseSession_SteersAndIsIdempotent(t *testing.T) {
	svc, _, _, _ := newCommandService()
	hub := operator.NewExecutionControlHub()
	c := &fakeControls{}
	hub.Register("s1", c)
	svc.SetSteeringSources(hub, &fakeApprovalHub{})

	req := &pb.SessionCommandRequest{CommandId: "p1", Reason: "investigate", SessionId: "s1"}
	if _, err := svc.PauseSession(opCtx(), req); err != nil {
		t.Fatalf("pause: %v", err)
	}
	ack, err := svc.PauseSession(opCtx(), req) // retry
	if err != nil || !ack.GetDeduped() {
		t.Fatalf("retry should dedup, got ack=%+v err=%v", ack, err)
	}
	if c.paused != 1 {
		t.Fatalf("expected exactly one Pause despite retry, got %d", c.paused)
	}
}

// A command for a session with no live execution fails cleanly.
func TestPauseSession_NoLiveExecution(t *testing.T) {
	svc, _, _, _ := newCommandService()
	svc.SetSteeringSources(operator.NewExecutionControlHub(), &fakeApprovalHub{})

	_, err := svc.PauseSession(opCtx(), &pb.SessionCommandRequest{CommandId: "p1", Reason: "x", SessionId: "ghost"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition for no live execution, got %v", err)
	}
}

// InjectCorrection reaches the live execution via the control hub and delivers
// the instruction; no live execution → FailedPrecondition. ADR-0047 0047-22.
func TestInjectCorrection_DeliversViaControlHub(t *testing.T) {
	svc, _, _, _ := newCommandService()
	hub := operator.NewExecutionControlHub()
	c := &fakeControls{}
	hub.Register("s1", c)
	svc.SetSteeringSources(hub, &fakeApprovalHub{})

	if _, err := svc.InjectCorrection(opCtx(), &pb.InjectCorrectionRequest{
		CommandId: "i1", Reason: "redirect", SessionId: "s1", Instruction: "do X instead",
	}); err != nil {
		t.Fatalf("inject: %v", err)
	}
	if len(c.injected) != 1 || c.injected[0] != "do X instead" {
		t.Fatalf("expected the instruction delivered to the live execution, got %v", c.injected)
	}

	_, err := svc.InjectCorrection(opCtx(), &pb.InjectCorrectionRequest{
		CommandId: "i2", Reason: "x", SessionId: "ghost", Instruction: "y",
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition for no live execution, got %v", err)
	}
}

// ResolveHITL submits the decision to the ApprovalHub once (idempotent).
func TestResolveHITL_SubmitsOnce(t *testing.T) {
	svc, _, _, _ := newCommandService()
	hub := &fakeApprovalHub{}
	svc.SetSteeringSources(operator.NewExecutionControlHub(), hub)

	req := &pb.ResolveHITLRequest{CommandId: "h1", Reason: "safe to proceed", InterventionId: "iv-1", Approve: true}
	if _, err := svc.ResolveHITL(opCtx(), req); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := svc.ResolveHITL(opCtx(), req); err != nil { // retry
		t.Fatalf("resolve retry: %v", err)
	}
	if len(hub.submits) != 1 || hub.submits[0] != "iv-1" {
		t.Fatalf("expected exactly one Submit for iv-1, got %+v", hub.submits)
	}
}
