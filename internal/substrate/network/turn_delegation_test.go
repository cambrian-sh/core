package network

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/goleak"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/metabolism/dispatch"
)

// T-20261106-a-chat-turn-that-fails-deadline-exceeded-or: a plan a chat turn
// delegated must end with the turn, its pending approval must be withdrawn, and no
// approval for it may be raised afterwards.

// turnFixture is an Execute fixture whose one step calls a dangerous tool through
// the real ExecuteTool RPC, gated by the real in-memory approval controller with
// one operator watching.
type turnFixture struct {
	s         *Server
	gw        *SubstrateLLMGateway
	approvals <-chan domain.ApprovalRequest
	stopWatch func()
	turnLease domain.LeaseID
}

func newTurnFixture(t *testing.T, caller callerFunc) *turnFixture {
	t.Helper()
	s := watchdogServer(t, 2, config.PlanConfig{PlanIdleTimeoutMs: 60000, PlanMaxDurationMs: 60000}, caller, nil)
	gw := s.LLMGateway.(*SubstrateLLMGateway)
	reg := domain.NewInMemoryToolRegistry()
	reg.Register(domain.SystemTool{Name: "execute_code", Dangerous: true})
	grants := domain.NewInMemoryGrantsStore()
	grants.Set("worker", []domain.ToolGrant{{Tool: "execute_code", Policy: domain.ToolResourcePolicy{AllowAll: true}}})
	hub := domain.NewAddressedHub(domain.AddressedHubConfig{DefaultTimeout: 30 * time.Second})
	s.ToolExecutor = newTestToolExecutor(t, domain.ToolExecutorControls{Registry: reg, Grants: grants, Approval: hub},
		domain.ToolExecutor{Handler: staticToolHandler{result: []byte(`{"ok":1}`)}})
	s.Delegations = NewTurnDelegations()
	// Bind each step lease to the agent it is dispatched to, as production does.
	s.Dispatcher.(*dispatch.Dispatcher).Leases = gw
	ch, stop := opRequests(hub)
	t.Cleanup(stop)

	// The chat turn's lease, bound the way TurnService binds it.
	lease, err := gw.Acquire(context.Background(), domain.StepAllocation{}, 8192, time.Minute)
	if err != nil {
		t.Fatalf("acquire turn lease: %v", err)
	}
	gw.BindLease(lease, domain.LeaseBinding{ConversationID: "c1", OriginMessageID: "m1", AgentID: "chat_agent"})
	return &turnFixture{s: s, gw: gw, approvals: ch, stopWatch: stop, turnLease: lease}
}

// execute runs the delegated plan the way the chat worker's `delegate` does: an
// Execute presenting the turn lease, on a context the turn does NOT control.
func (f *turnFixture) execute() (*pb.Handoff, error) {
	ctx := agentPlaneCtx("chat_agent", leaseHeader, string(f.turnLease))
	return f.s.Execute(ctx, &pb.Handoff{Payload: &pb.Object{Data: []byte("file my receipts")}})
}

func (f *turnFixture) callTool(lease domain.LeaseID) *pb.ExecuteToolResponse {
	return f.callToolAs("worker", lease)
}

func (f *turnFixture) callToolAs(agentID string, lease domain.LeaseID) *pb.ExecuteToolResponse {
	resp, err := f.s.ExecuteTool(ctxWithAgentID(agentID), &pb.ExecuteToolRequest{
		ToolName: "execute_code", ArgsJson: `{"code":"print(1)"}`, LeaseId: string(lease),
	})
	if err != nil {
		panic(err)
	}
	return resp
}

func runTurnEndScenario(t *testing.T) {
	stepLeases := make(chan domain.LeaseID, 4)
	afterEnd := make(chan *pb.ExecuteToolResponse, 1)
	var f *turnFixture
	caller := callerFunc(func(ctx context.Context, agentID string, h *domain.Handoff, _ string) (*domain.Handoff, error) {
		lease := stepLease(h)
		select {
		case stepLeases <- lease:
		default:
		}
		// The step's agent asks for a dangerous tool and waits on the operator.
		first := f.callTool(lease)
		if !first.Denied {
			return nil, errors.New("the approval must not be granted")
		}
		// An agent loop does not stop at one refusal: it tries again. The ordering
		// guarantee is that this second call raises NO approval.
		select {
		case afterEnd <- f.callTool(lease):
		default:
		}
		return nil, errors.New(first.DenyReason)
	})
	f = newTurnFixture(t, caller)
	defer f.stopWatch() // before a deferred goleak check, not only at Cleanup

	type result struct {
		resp *pb.Handoff
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := f.execute()
		done <- result{resp, err}
	}()

	var raised domain.ApprovalRequest
	select {
	case raised = <-f.approvals:
	case <-time.After(10 * time.Second):
		t.Fatal("the step never raised its approval")
	}
	// The plan is the conversation's live work while it waits on the operator.
	if f.s.Delegations.ConversationActivity("c1").IsZero() {
		t.Fatal("a delegated plan waiting on an approval must read as live work")
	}

	// The turn ends (here: its 240 s deadline).
	start := time.Now()
	f.s.Delegations.EndTurn("c1", "m1", f.turnLease, context.DeadlineExceeded)

	second := <-afterEnd
	if !second.Denied || second.DenyReason != ErrTurnEnded.Error() {
		t.Fatalf("a tool call after the turn ended must be refused as ended, got %+v", second)
	}
	var r result
	select {
	case r = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the delegated plan kept running after its turn ended")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the plan took %s to stop", took)
	}
	if r.err == nil && (r.resp == nil || r.resp.Metadata["_partial_plan"] != "true") {
		t.Fatalf("a cancelled plan must not report success: %+v", r.resp)
	}
	select {
	case extra := <-f.approvals:
		t.Fatalf("an approval was raised after the turn ended (first was %s): %+v", raised.ID, extra)
	default:
	}

	// The step lease is Completed by now; presenting it again is still refused,
	// before any approval, and the plan never reached step 2.
	lease := <-stepLeases
	if late := f.callTool(lease); !late.Denied || late.DenyReason != ErrTurnEnded.Error() {
		t.Fatalf("a completed step lease of an ended turn must be refused, got %+v", late)
	}
	if late := f.callToolAs("chat_agent", f.turnLease); !late.Denied || late.DenyReason != ErrTurnEnded.Error() {
		t.Fatalf("the ended turn's own lease must be refused, got %+v", late)
	}
	select {
	case l := <-stepLeases:
		t.Fatalf("a step was dispatched after the turn ended (lease %s)", l)
	default:
	}
	select {
	case extra := <-f.approvals:
		t.Fatalf("an approval was raised after the turn ended: %+v", extra)
	default:
	}

	// A late Execute from the same turn is refused outright.
	if _, err := f.execute(); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("a late delegation from an ended turn must be refused, got %v", err)
	}
	if !f.s.Delegations.ConversationActivity("c1").IsZero() {
		t.Fatal("an ended turn has no live work")
	}
}

func TestExecute_TurnEndCancelsPlanWithdrawsApprovalAndRaisesNoMore(t *testing.T) {
	t.Parallel()
	runTurnEndScenario(t)
}

// Not parallel: goleak sees the whole process.
func TestTurnEnd_LeaksNoGoroutines(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	runTurnEndScenario(t)
}

// Work no chat turn ordered is untouched: no registration, no guard.
func TestTurnDelegations_WorkWithoutATurnPassesThrough(t *testing.T) {
	t.Parallel()
	d := NewTurnDelegations()
	ctx, run, err := d.BeginRun(t.Context(), domain.LeaseBinding{SessionID: "s1"})
	if err != nil || run != nil || ctx != t.Context() {
		t.Fatalf("no origin: want pass-through, got run=%v err=%v", run, err)
	}
	run.Attach("r", nil) // nil-safe
	run.AddLease("l")
	run.End()
	gctx, release, ok := d.GuardToolCall(t.Context(), "l", domain.LeaseBinding{RunID: "unknown"}, true)
	defer release()
	if !ok || gctx != t.Context() {
		t.Fatal("a lease of no turn must pass through")
	}
	var nilReg *TurnDelegations
	nilReg.EndTurn("c", "m", "l", nil)
	if !nilReg.ConversationActivity("c").IsZero() {
		t.Fatal("nil registry reports no activity")
	}
}

// A nested Execute (a step agent of the turn's plan delegating again) is the
// same turn's work, and ends with it.
func TestTurnDelegations_NestedRunEndsWithTheTurn(t *testing.T) {
	t.Parallel()
	d := NewTurnDelegations()
	_, outer, err := d.BeginRun(t.Context(), domain.LeaseBinding{ConversationID: "c1", OriginMessageID: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	defer outer.End()
	// Still planning: no probe yet, reads as active.
	if d.ConversationActivity("c1").IsZero() {
		t.Fatal("a planning run is live work")
	}
	at := time.Now().Add(-time.Hour)
	outer.Attach("run-1", func() time.Time { return at })
	if got := d.ConversationActivity("c1"); !got.Equal(at) {
		t.Fatalf("want the plan's own probe %s, got %s", at, got)
	}
	if !d.ConversationActivity("other").IsZero() {
		t.Fatal("another conversation has no work")
	}
	nctx, nested, err := d.BeginRun(t.Context(), domain.LeaseBinding{RunID: "run-1"})
	if err != nil || nested == nil {
		t.Fatalf("nested run must register under the turn: %v", err)
	}
	defer nested.End()
	d.EndTurn("c1", "m1", "", nil)
	if !errors.Is(context.Cause(nctx), ErrTurnEnded) {
		t.Fatalf("nested run not cancelled with the turn: %v", context.Cause(nctx))
	}
	if _, _, err := d.BeginRun(t.Context(), domain.LeaseBinding{RunID: "run-1"}); !errors.Is(err, ErrTurnEnded) {
		t.Fatalf("a delegation from an ended turn's run must be refused, got %v", err)
	}
	// A step lease minted by the run after the turn ended is revoked at once.
	outer.AddLease("late-step")
	if _, _, ok := d.GuardToolCall(t.Context(), "late-step", domain.LeaseBinding{}, false); ok {
		t.Fatal("a lease minted after the turn ended must be refused")
	}
}

// Tombstones are bounded: an ended turn is forgotten after the retention window.
func TestTurnDelegations_TombstonesArePruned(t *testing.T) {
	t.Parallel()
	d := NewTurnDelegations()
	now := time.Now()
	d.now = func() time.Time { return now }
	d.EndTurn("c1", "m1", "lease-1", nil)
	now = now.Add(2 * tombstoneRetention)
	d.EndTurn("c2", "m2", "", nil)
	if _, ok := d.ended["m1"]; ok {
		t.Fatal("ended-turn tombstone not pruned")
	}
	if _, ok := d.revoked["lease-1"]; ok {
		t.Fatal("revoked-lease tombstone not pruned")
	}
}
