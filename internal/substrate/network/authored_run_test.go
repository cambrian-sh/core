package network

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/substrate/session"
	"google.golang.org/grpc/metadata"
)

// ENG-2246: under the single-agent lane an authored plan (SubmitPlan,
// PlanExecutor) is run as one brief; its run is persisted and its session closed
// with the outcome — success or failure — instead of staying active for ever
// with no run row.
func authoredLaneServer(t *testing.T, fake *fakeAuctioneer) (*Server, *session.SessionManager, *memRunStore, domain.SessionID) {
	t.Helper()
	s, _ := newLeaseServer(t)
	mgr := session.New(newMemSessionStore())
	s.SessionMgr = mgr
	s.Router = refusingRouter{t: t}
	s.Delegations = NewTurnDelegations()
	s.Dispatcher = fake
	runs := newMemRunStore()
	s.Runs = runs
	s.ExecCfg = config.ExecutionConfig{Lane: config.LaneConfig{Default: config.LaneSingleAgent,
		SingleAgent: config.SingleAgentLaneConfig{AgentID: "assistant", MaxToolRounds: 7, MaxWallSeconds: 60}}}
	ses, err := mgr.CreateScopedSession(context.Background(), "readiness note", "", domain.TagSet{})
	if err != nil {
		t.Fatal(err)
	}
	return s, mgr, runs, ses.ID
}

func authoredHandoff(t *testing.T) *pb.Handoff {
	t.Helper()
	b, err := json.Marshal(domain.ExecutionPlan{Subject: "readiness note", Steps: []domain.Step{
		{Query: "Summarise in one sentence: 14 damaged cartons", PreferredAgent: "assistant", AgentPin: "hard"}}})
	if err != nil {
		t.Fatal(err)
	}
	return &pb.Handoff{Payload: &pb.Object{Data: []byte("")}, Metadata: map[string]string{AuthoredPlanMetadataKey: string(b)}}
}

// opCtx presents the session the way the operator plane does
// (app.operatorRunContext: x-session-id).
func opCtx(sid domain.SessionID) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-session-id", string(sid)))
}

func onlyRun(t *testing.T, runs *memRunStore, sid domain.SessionID) domain.Run {
	t.Helper()
	rs, _ := runs.ListRunsForSession(context.Background(), sid)
	if len(rs) != 1 {
		t.Fatalf("runs for the session: %+v, want exactly one", rs)
	}
	return rs[0]
}

func TestAuthoredPlan_SingleAgentLane_SuccessWritesTheRunAndClosesTheSession(t *testing.T) {
	t.Parallel()
	fake := &fakeAuctioneer{resp: &domain.Handoff{Payload: &domain.Payload{Data: []byte("Step 1 complete")}}}
	s, mgr, runs, sid := authoredLaneServer(t, fake)
	if _, err := s.Execute(opCtx(sid), authoredHandoff(t)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	r := onlyRun(t, runs, sid)
	if r.Status != domain.RunCompleted || r.EndedAt.IsZero() || r.Subject != "readiness note" || r.Plan != nil || r.Resumable() {
		t.Fatalf("run %+v, want completed, ended, subject kept, no plan (never resumable in this lane)", r)
	}
	ses, _ := mgr.GetSession(context.Background(), sid)
	if ses.Status != domain.SessionCompleted {
		t.Fatalf("session %s, want completed", ses.Status)
	}
}

func TestAuthoredPlan_SingleAgentLane_FailureWritesTheRunAndClosesTheSession(t *testing.T) {
	t.Parallel()
	fake := &fakeAuctioneer{err: errors.New("provider: Unauthenticated")}
	s, mgr, runs, sid := authoredLaneServer(t, fake)
	if _, err := s.Execute(opCtx(sid), authoredHandoff(t)); err == nil {
		t.Fatal("the agent failed: Execute must fail")
	}
	if r := onlyRun(t, runs, sid); r.Status != domain.RunFailed || r.EndedAt.IsZero() {
		t.Fatalf("run %+v, want failed", r)
	}
	ses, _ := mgr.GetSession(context.Background(), sid)
	if ses.Status != domain.SessionCompleted {
		t.Fatalf("session %s, want closed with the failure", ses.Status)
	}
}

// An ordinary single-agent request (no authored plan) keeps its session open and
// writes no run row: chat-ordered work continues in its session.
func TestSingleAgentLane_OrdinaryRequestLeavesTheSessionAlone(t *testing.T) {
	t.Parallel()
	fake := &fakeAuctioneer{resp: &domain.Handoff{Payload: &domain.Payload{Data: []byte("ok")}}}
	s, mgr, runs, sid := authoredLaneServer(t, fake)
	if _, err := s.Execute(opCtx(sid), &pb.Handoff{Payload: &pb.Object{Data: []byte("tidy my receipts")}}); err != nil {
		t.Fatal(err)
	}
	if rs, _ := runs.ListRunsForSession(context.Background(), sid); len(rs) != 0 {
		t.Fatalf("runs %+v, want none", rs)
	}
	if ses, _ := mgr.GetSession(context.Background(), sid); ses.Status != domain.SessionActive {
		t.Fatalf("session %s, want active", ses.Status)
	}
}
