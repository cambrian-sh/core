package network

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/substrate/operator"
	"github.com/cambrian-sh/core/internal/substrate/session"
)

// blockingAgent stands for an agent mid-run: it reports the run it was handed
// and then works until its context ends.
type blockingAgent struct {
	*fakeAuctioneer
	started chan *domain.Handoff
}

func (b blockingAgent) CallAgent(ctx context.Context, _ string, h *domain.Handoff, _ string) (*domain.Handoff, error) {
	b.started <- h
	<-ctx.Done()
	return nil, context.Cause(ctx)
}

// E2E through the real path (contract 0102,
// T-20261106-operator-closesession-only-pauses-a-live-plan): an operator's
// CancelRun, addressed by the run id the lane minted, stops a single-agent run
// in flight; Execute answers Canceled naming who stopped it and why, and the
// run is no longer tracked.
func TestCancelRun_StopsALiveSingleAgentRun(t *testing.T) {
	t.Parallel()
	s, gw := newLeaseServer(t)
	s.SessionMgr = session.New(newMemSessionStore())
	s.Router = refusingRouter{t: t}
	hub := operator.NewExecutionControlHub()
	s.ControlHub = hub
	agent := blockingAgent{fakeAuctioneer: &fakeAuctioneer{}, started: make(chan *domain.Handoff, 1)}
	s.Dispatcher = agent
	s.ExecCfg = config.ExecutionConfig{Lane: config.LaneConfig{Default: config.LaneSingleAgent,
		SingleAgent: config.SingleAgentLaneConfig{AgentID: "worker", MaxToolRounds: 7, MaxWallSeconds: 60}}}

	ops := operator.NewService(operator.NewSpool(operator.SpoolConfig{}))
	ops.SetCommandSources(operator.NewInMemoryAuditStore(), domain.NewInMemoryGrantsStore())
	ops.SetSteeringSources(hub, nil)

	type result struct {
		resp *pb.Handoff
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := s.Execute(context.Background(), &pb.Handoff{Payload: &pb.Object{Type: "task", Data: []byte("reconcile every invoice")}})
		done <- result{resp, err}
	}()

	var h *domain.Handoff
	select {
	case h = <-agent.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the agent never started")
	}
	b, ok := gw.ResolveLease(domain.LeaseID(h.Context["_session_token_id"]))
	if !ok || b.RunID == "" {
		t.Fatalf("no run bound to the lane's lease: %+v", b)
	}
	// The actor comes from the operator interceptor in production; this handler
	// call has none, so the cause names an empty actor and the reason.
	opCtx := context.Background()
	if _, err := ops.CancelRun(opCtx, &pb.CancelRunRequest{CommandId: "c1", Reason: "wrong ledger", RunId: string(b.RunID)}); err != nil {
		t.Fatalf("CancelRun: %v", err)
	}

	var r result
	select {
	case r = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Execute did not stop after CancelRun")
	}
	if status.Code(r.err) != codes.Canceled || !strings.Contains(r.err.Error(), "wrong ledger") {
		t.Fatalf("Execute = %v, %v; want Canceled naming the reason", r.resp, r.err)
	}
	if _, err := ops.CancelRun(opCtx, &pb.CancelRunRequest{CommandId: "c2", Reason: "again", SessionId: string(b.SessionID)}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("a finished run is still live: %v", err)
	}
}

// The planner lane records an operator-cancelled run as cancelled, which makes
// it not resumable.
func TestMarkRunCancelled_MakesTheRunNotResumable(t *testing.T) {
	t.Parallel()
	runs := newMemRunStore()
	plan := &domain.ExecutionPlan{Steps: []domain.Step{{Query: "a"}}}
	if err := runs.SaveRun(domain.Run{ID: "r1", SessionID: "s1", Status: domain.RunRunning, Plan: plan}); err != nil {
		t.Fatal(err)
	}
	s := &Server{Runs: runs}
	s.markRunCancelled("r1", &operator.RunCancelledError{Actor: "alice", Reason: "stop"})
	got, _ := runs.GetRun("r1")
	if got.Status != domain.RunCancelled || got.Resumable() || got.EndedAt.IsZero() {
		t.Fatalf("run = %+v", got)
	}
	if _, err := s.ResumeRun(context.Background(), "r1", "s1"); err == nil {
		t.Fatal("a cancelled run was resumed")
	}
	s.markRunCancelled("ghost", &operator.RunCancelledError{}) // unknown: logged, no panic
}
