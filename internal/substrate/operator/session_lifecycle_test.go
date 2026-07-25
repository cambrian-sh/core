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

// statusRecorder captures the lifecycle transitions the steering commands persist.
type statusRecorder struct {
	calls []struct {
		session string
		status  domain.SessionStatus
		reason  string
	}
	err error
}

func (r *statusRecorder) ops() operator.SessionOpsFuncs {
	return operator.SessionOpsFuncs{
		SetStatusFn: func(_ context.Context, sessionID string, st domain.SessionStatus, reason string) error {
			if r.err != nil {
				return r.err
			}
			r.calls = append(r.calls, struct {
				session string
				status  domain.SessionStatus
				reason  string
			}{sessionID, st, reason})
			return nil
		},
	}
}

// steerControls is a no-op live execution so pause/resume find something to steer.
type steerControls struct{ paused, resumed int }

func (c *steerControls) Pause()  { c.paused++ }
func (c *steerControls) Resume() { c.resumed++ }
func (c *steerControls) Inject(string) error {
	return nil
}

// Pausing must PERSIST the status, not merely nudge the in-memory executor. Without this the
// durable status stayed "active" forever, so the console showed a paused session as running
// and its Resume control was permanently unreachable.
func TestPauseSession_PersistsStatus(t *testing.T) {
	svc, _, _, _ := newCommandService()
	svc.SetCommandEffects(operator.NoopEffects{})
	hub := operator.NewExecutionControlHub()
	ctrl := &steerControls{}
	hub.Register("sess-1", ctrl)
	svc.SetSteeringSources(hub, &fakeApprovalHub{})
	rec := &statusRecorder{}
	svc.SetSessionOps(rec.ops())

	ack, err := svc.PauseSession(opCtx(), &pb.SessionCommandRequest{
		CommandId: "p1", Reason: "customer escalation", SessionId: "sess-1",
	})
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	if ack.GetDeduped() {
		t.Error("first pause must not dedup")
	}
	if ctrl.paused != 1 {
		t.Errorf("live execution not steered: paused=%d", ctrl.paused)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("expected 1 persisted transition, got %d", len(rec.calls))
	}
	if rec.calls[0].status != domain.SessionPaused {
		t.Errorf("persisted status = %q, want paused", rec.calls[0].status)
	}
	if rec.calls[0].reason != "customer escalation" {
		t.Errorf("reason = %q, want the operator's justification", rec.calls[0].reason)
	}
}

func TestResumeSession_PersistsActive(t *testing.T) {
	svc, _, _, _ := newCommandService()
	svc.SetCommandEffects(operator.NoopEffects{})
	hub := operator.NewExecutionControlHub()
	ctrl := &steerControls{}
	hub.Register("sess-2", ctrl)
	svc.SetSteeringSources(hub, &fakeApprovalHub{})
	rec := &statusRecorder{}
	svc.SetSessionOps(rec.ops())

	if _, err := svc.ResumeSession(opCtx(), &pb.SessionCommandRequest{
		CommandId: "r1", Reason: "escalation resolved", SessionId: "sess-2",
	}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if ctrl.resumed != 1 {
		t.Errorf("live execution not resumed: resumed=%d", ctrl.resumed)
	}
	if len(rec.calls) != 1 || rec.calls[0].status != domain.SessionActive {
		t.Fatalf("expected one persisted transition to active, got %+v", rec.calls)
	}
}

// A retry must not transition twice.
func TestPauseSession_IdempotentOnCommandID(t *testing.T) {
	svc, _, _, _ := newCommandService()
	svc.SetCommandEffects(operator.NoopEffects{})
	hub := operator.NewExecutionControlHub()
	hub.Register("sess-3", &steerControls{})
	svc.SetSteeringSources(hub, &fakeApprovalHub{})
	rec := &statusRecorder{}
	svc.SetSessionOps(rec.ops())

	req := &pb.SessionCommandRequest{CommandId: "p-dup", Reason: "why", SessionId: "sess-3"}
	if _, err := svc.PauseSession(opCtx(), req); err != nil {
		t.Fatalf("pause: %v", err)
	}
	ack, err := svc.PauseSession(opCtx(), req)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !ack.GetDeduped() {
		t.Error("retry should report deduped")
	}
	if len(rec.calls) != 1 {
		t.Errorf("a retry must not transition twice, got %d", len(rec.calls))
	}
}

// Closing seals the session. It deliberately does NOT require a live execution: the common
// case is sealing work that has already stopped, and requiring an executor would make a
// finished session impossible to close — which is how the lifecycle ended up with no
// terminator at all.
func TestCloseSession_CompletesWithoutLiveExecution(t *testing.T) {
	svc, _, _, _ := newCommandService()
	svc.SetCommandEffects(operator.NoopEffects{})
	svc.SetSteeringSources(operator.NewExecutionControlHub(), &fakeApprovalHub{}) // empty hub
	rec := &statusRecorder{}
	svc.SetSessionOps(rec.ops())

	ack, err := svc.CloseSession(opCtx(), &pb.SessionCommandRequest{
		CommandId: "c1", Reason: "work delivered", SessionId: "sess-9",
	})
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if ack.GetDeduped() {
		t.Error("first close must not dedup")
	}
	if len(rec.calls) != 1 || rec.calls[0].status != domain.SessionCompleted {
		t.Fatalf("expected a transition to completed, got %+v", rec.calls)
	}
	if rec.calls[0].reason != "work delivered" {
		t.Errorf("reason = %q", rec.calls[0].reason)
	}
}

func TestCloseSession_ValidatesRequiredFields(t *testing.T) {
	svc, _, _, _ := newCommandService()
	svc.SetCommandEffects(operator.NoopEffects{})
	svc.SetSessionOps((&statusRecorder{}).ops())

	for name, req := range map[string]*pb.SessionCommandRequest{
		"no reason":     {CommandId: "c", SessionId: "s"},
		"no command_id": {Reason: "r", SessionId: "s"},
		"no session_id": {CommandId: "c", Reason: "r"},
	} {
		if _, err := svc.CloseSession(opCtx(), req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: expected InvalidArgument, got %v", name, err)
		}
	}
}

// An unwired SessionOps must say so rather than silently succeeding.
func TestCloseSession_UnwiredIsUnimplemented(t *testing.T) {
	svc, _, _, _ := newCommandService()
	svc.SetCommandEffects(operator.NoopEffects{})
	svc.SetSessionOps(operator.SessionOpsFuncs{}) // no SetStatusFn

	_, err := svc.CloseSession(opCtx(), &pb.SessionCommandRequest{
		CommandId: "c2", Reason: "done", SessionId: "s1",
	})
	if status.Code(err) != codes.FailedPrecondition && status.Code(err) != codes.Unimplemented {
		t.Fatalf("expected an honest failure for unwired close, got %v", err)
	}
}
