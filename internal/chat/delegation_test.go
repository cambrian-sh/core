package chat

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/substrate/network"
	"github.com/cambrian-sh/core/internal/testing/fakemodels"
	"google.golang.org/grpc/metadata"
)

// The turn ↔ delegated-plan lifetime (T-20261106-a-chat-turn-that-fails-deadline-
// exceeded-or) and the stall watchdog's view of the delegated plan
// (T-20261106-chat-stall-detection-cancels-a-turn-whose), driven through the real
// TurnService, gateway leases and network.TurnDelegations.

// delegatingPool is a session worker that delegates the turn to a plan the way the
// SDK's `delegate` does: an Execute presenting the turn lease, on a context the
// turn does NOT control (the worker's own RPC). The plan body is the test's.
type delegatingPool struct {
	gw      *network.SubstrateLLMGateway
	d       *network.TurnDelegations
	plan    func(ctx context.Context, run *network.DelegatedRun) error
	started chan struct{} // closed once the plan is registered
	ended   chan error    // the plan context's cause when the plan body returned
	// lease and binding are the turn lease the worker presented; set before started.
	lease   domain.LeaseID
	binding domain.LeaseBinding
}

func newDelegatingPool(gw *network.SubstrateLLMGateway, d *network.TurnDelegations, plan func(context.Context, *network.DelegatedRun) error) *delegatingPool {
	return &delegatingPool{gw: gw, d: d, plan: plan, started: make(chan struct{}), ended: make(chan error, 1)}
}

func (p *delegatingPool) Dispatch(ctx context.Context, h *domain.Handoff) (*domain.Handoff, error) {
	p.lease = domain.LeaseID(h.Context["_session_token_id"])
	b, _ := p.gw.ResolveLease(p.lease)
	p.binding = b
	runCtx, run, err := p.d.BeginRun(context.Background(), b)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, errors.New("the turn lease carried no origin")
	}
	planDone := make(chan error, 1)
	go func() {
		defer run.End()
		close(p.started)
		perr := p.plan(runCtx, run)
		p.ended <- context.Cause(runCtx)
		planDone <- perr
	}()
	select {
	case err := <-planDone:
		if err != nil {
			return nil, err
		}
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("filed")}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// waitEnded returns the plan's cancellation cause, failing when the plan outlives
// its turn.
func (p *delegatingPool) waitEnded(t *testing.T) error {
	t.Helper()
	select {
	case cause := <-p.ended:
		return cause
	case <-time.After(5 * time.Second):
		t.Fatal("the delegated plan kept running after its turn ended")
		return nil
	}
}

// delegationService is a TurnService wired as app.go wires it: real gateway
// leases bound to the conversation, the delegation registry as the turn ender,
// and the liveness probe joining progress (frozen: nothing reports), the
// gateway's view of the turn lease, and the delegated plans' progress.
func delegationService(t *testing.T, gw *network.SubstrateLLMGateway, d *network.TurnDelegations, pool Dispatcher) *TurnService {
	t.Helper()
	svc := inflightService(t, gw, pool)
	svc.SetDelegationEnder(d)
	frozen := time.Now()
	svc.SetLivenessProbe(LatestActivity(func(string) time.Time { return frozen }, gw.ConversationActivity, d.ConversationActivity))
	return svc
}

func newGateway() *network.SubstrateLLMGateway {
	gw := network.NewLLMGateway(config.DefaultConfig().Execution)
	gw.SetStepModels(fakemodels.New())
	return gw
}

// fakeClock advances a fixed step every time the stall watchdog reads it, so a
// test covers minutes of turn time in milliseconds.
type fakeClock struct {
	mu   sync.Mutex
	t    time.Time
	step time.Duration
}

func (c *fakeClock) tick() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(c.step)
	return c.t
}

func (c *fakeClock) peek() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// A turn cancelled at its deadline ends the plan it delegated, revokes its lease
// and refuses any later delegation from it.
func TestTurn_DeadlineEndsTheDelegatedPlan(t *testing.T) {
	gw, d := newGateway(), network.NewTurnDelegations()
	pool := newDelegatingPool(gw, d, func(ctx context.Context, _ *network.DelegatedRun) error {
		<-ctx.Done() // a long plan, still working when the turn gives up
		return ctx.Err()
	})
	svc := delegationService(t, gw, d, pool)
	svc.StallTimeout = 0 // the deadline, not the stall, ends this turn
	svc.TurnTimeout = 300 * time.Millisecond

	_, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "file my receipts"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
	if cause := pool.waitEnded(t); !errors.Is(cause, network.ErrTurnEnded) {
		t.Fatalf("plan must be cancelled because its turn ended, cause %v", cause)
	}
	if pool.binding.OriginMessageID == "" {
		t.Fatal("the turn lease must carry its origin message")
	}
	if _, _, ok := d.GuardToolCall(context.Background(), pool.lease, domain.LeaseBinding{}, false); ok {
		t.Fatal("the ended turn's lease must be refused at the tool gate")
	}
	if _, _, err := d.BeginRun(context.Background(), pool.binding); !errors.Is(err, network.ErrTurnEnded) {
		t.Fatalf("a late delegation from the ended turn must be refused, got %v", err)
	}
}

// A plan that keeps making progress for well over the 90 s stall window (a step
// boundary every 30 s of fake time, 150 s in all) keeps its turn alive: the stall
// watchdog reads the delegated plan's own liveness probe.
func TestTurn_LongDelegatedPlanWithSteadyProgressIsNotAStall(t *testing.T) {
	gw, d := newGateway(), network.NewTurnDelegations()
	clock := &fakeClock{t: time.Now(), step: 5 * time.Second}
	start := clock.peek()
	pool := newDelegatingPool(gw, d, func(ctx context.Context, run *network.DelegatedRun) error {
		run.Attach("run-1", func() time.Time {
			elapsed := clock.peek().Sub(start)
			return start.Add(elapsed - elapsed%(30*time.Second)) // the last step boundary
		})
		for clock.peek().Sub(start) < 150*time.Second {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Millisecond):
			}
		}
		return nil
	})
	svc := delegationService(t, gw, d, pool)
	svc.StallTimeout = DefaultStallTimeout
	svc.TurnTimeout = 30 * time.Second
	svc.now, svc.stallPoll = clock.tick, 2*time.Millisecond

	got, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "analyse the bank CSV"})
	if err != nil {
		t.Fatalf("a delegated plan making steady progress must not be cut: %v", err)
	}
	if got.Content != "filed" {
		t.Fatalf("reply %q", got.Content)
	}
	if cause := pool.waitEnded(t); cause != nil {
		t.Fatalf("a plan that finished was cancelled: %v", cause)
	}
}

// A delegated plan that genuinely stops moving is still a stall: the turn is cut
// after the window, and the plan ends with it.
func TestTurn_IdleDelegatedPlanIsCancelled(t *testing.T) {
	gw, d := newGateway(), network.NewTurnDelegations()
	clock := &fakeClock{t: time.Now(), step: 5 * time.Second}
	start := clock.peek()
	pool := newDelegatingPool(gw, d, func(ctx context.Context, run *network.DelegatedRun) error {
		run.Attach("run-1", func() time.Time { return start }) // nothing moves after the start
		<-ctx.Done()
		return ctx.Err()
	})
	svc := delegationService(t, gw, d, pool)
	svc.StallTimeout = DefaultStallTimeout
	svc.TurnTimeout = 30 * time.Second
	svc.now, svc.stallPoll = clock.tick, 2*time.Millisecond

	_, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "analyse"})
	if !errors.Is(err, ErrTurnStalled) {
		t.Fatalf("want ErrTurnStalled, got %v", err)
	}
	if silent := clock.peek().Sub(start); silent < DefaultStallTimeout {
		t.Fatalf("cut after %s of fake silence, before the %s window", silent, DefaultStallTimeout)
	}
	if cause := pool.waitEnded(t); !errors.Is(cause, network.ErrTurnEnded) {
		t.Fatalf("the idle plan must end with its stalled turn, cause %v", cause)
	}
}

// approvalPlan is a delegated plan whose one step calls a dangerous tool through
// the real ExecuteTool RPC and waits on the operator.
func approvalPlan(t *testing.T, gw *network.SubstrateLLMGateway, d *network.TurnDelegations, grace time.Duration) (*delegatingPool, <-chan domain.ApprovalRequest, *domain.AddressedHub) {
	t.Helper()
	reg := domain.NewInMemoryToolRegistry()
	reg.Register(domain.SystemTool{Name: "execute_code", Dangerous: true})
	grants := domain.NewInMemoryGrantsStore()
	grants.Set("worker", []domain.ToolGrant{{Tool: "execute_code", Policy: domain.ToolResourcePolicy{AllowAll: true}}})
	hub := domain.NewAddressedHub(domain.AddressedHubConfig{DefaultTimeout: grace})
	ch, stop := opRequests(hub)
	t.Cleanup(stop)
	srv := &network.Server{LLMGateway: gw, Delegations: d, ToolExecutor: &domain.ToolExecutor{
		Registry: reg, Grants: grants, Approval: hub, Handler: okTool{},
	}}
	pool := newDelegatingPool(gw, d, func(ctx context.Context, run *network.DelegatedRun) error {
		step, err := gw.Acquire(ctx, domain.StepAllocation{}, 8192, time.Minute)
		if err != nil {
			return err
		}
		defer func() { _, _ = gw.Complete(context.Background(), step) }()
		gw.BindLease(step, domain.LeaseBinding{RunID: "run-1", StepIndex: 0, AgentID: "worker"})
		run.AddLease(step)
		run.Attach("run-1", func() time.Time { return gw.RunActivity("run-1") })
		resp, err := srv.ExecuteTool(metadata.NewIncomingContext(ctx, metadata.Pairs("x-agent-id", "worker")),
			&pb.ExecuteToolRequest{ToolName: "execute_code", ArgsJson: `{}`, LeaseId: string(step)})
		if err != nil {
			return err
		}
		if resp.Denied {
			// The agent's loop goes quiet after a refusal it cannot act on.
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	})
	return pool, ch, hub
}

type okTool struct{}

func (okTool) Execute(context.Context, domain.ToolCall) ([]byte, error) {
	return []byte(`{"ok":1}`), nil
}

// Waiting on a human approval is not a stall: the tool call waiting on it is work
// in flight on the plan's step lease. The operator answers after four stall
// windows and the turn completes.
func TestTurn_PendingApprovalIsNotAStall(t *testing.T) {
	gw, d := newGateway(), network.NewTurnDelegations()
	pool, approvals, hub := approvalPlan(t, gw, d, 30*time.Second)
	svc := delegationService(t, gw, d, pool)
	svc.StallTimeout = 300 * time.Millisecond
	svc.stallPoll = 20 * time.Millisecond

	go func() {
		r := <-approvals
		time.Sleep(1200 * time.Millisecond)
		opSubmit(hub, r.ID, true, "operator")
	}()
	got, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "chart it"})
	if err != nil {
		t.Fatalf("a turn whose plan waits on an approval must not stall: %v", err)
	}
	if got.Content != "filed" {
		t.Fatalf("reply %q", got.Content)
	}
	if cause := pool.waitEnded(t); cause != nil {
		t.Fatalf("plan cancelled: %v", cause)
	}
}

// The pause is bounded by the approval's own timeout: once an unanswered approval
// fails closed and the plan goes quiet, the turn stalls and the plan ends.
func TestTurn_UnansweredApprovalIsBoundedByItsTimeout(t *testing.T) {
	gw, d := newGateway(), network.NewTurnDelegations()
	pool, approvals, _ := approvalPlan(t, gw, d, 600*time.Millisecond)
	svc := delegationService(t, gw, d, pool)
	svc.StallTimeout = 300 * time.Millisecond
	svc.stallPoll = 20 * time.Millisecond

	go func() { <-approvals }() // an operator who never answers
	start := time.Now()
	_, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "chart it"})
	if !errors.Is(err, ErrTurnStalled) {
		t.Fatalf("want ErrTurnStalled once the approval timed out, got %v", err)
	}
	if el := time.Since(start); el < 600*time.Millisecond {
		t.Fatalf("stalled after %s, inside the approval's 600 ms timeout", el)
	}
	if cause := pool.waitEnded(t); !errors.Is(cause, network.ErrTurnEnded) {
		t.Fatalf("plan must end with its turn, cause %v", cause)
	}
}

// Closing the conversation cancels the turn running in it, and the plan that
// turn delegated.
func TestCloseConversation_CancelsTheTurnAndItsPlan(t *testing.T) {
	gw, d := newGateway(), network.NewTurnDelegations()
	pool := newDelegatingPool(gw, d, func(ctx context.Context, _ *network.DelegatedRun) error {
		<-ctx.Done()
		return ctx.Err()
	})
	svc := delegationService(t, gw, d, pool)
	svc.StallTimeout = 0

	go func() {
		<-pool.started
		if err := svc.CloseConversation(context.Background(), "c1"); err != nil {
			t.Errorf("close: %v", err)
		}
	}()
	_, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "file my receipts"})
	if !errors.Is(err, domain.ErrConversationClosed) {
		t.Fatalf("want ErrConversationClosed, got %v", err)
	}
	if cause := pool.waitEnded(t); !errors.Is(cause, network.ErrTurnEnded) {
		t.Fatalf("plan must end when its conversation closes, cause %v", cause)
	}
	if _, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "again"}); !errors.Is(err, domain.ErrConversationClosed) {
		t.Fatalf("a closed conversation takes no turns, got %v", err)
	}
}
