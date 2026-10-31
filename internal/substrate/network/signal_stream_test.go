package network

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	supwatcher "github.com/cambrian-sh/core/internal/supervision/watcher"
)

// fakeSignalStream feeds a fixed list of signals to SignalStream, then EOF.
type fakeSignalStream struct {
	grpc.ServerStream
	ctx  context.Context
	in   []*pb.Handoff
	sent []*pb.SymbiosisEvent
}

func (f *fakeSignalStream) Context() context.Context { return f.ctx }
func (f *fakeSignalStream) Recv() (*pb.Handoff, error) {
	if len(f.in) == 0 {
		return nil, io.EOF
	}
	h := f.in[0]
	f.in = f.in[1:]
	return h, nil
}
func (f *fakeSignalStream) Send(e *pb.SymbiosisEvent) error { f.sent = append(f.sent, e); return nil }

// tokenManager accepts exactly one daemon token.
type tokenManager struct{}

func (tokenManager) FindInstanceByToken(tok string) *domain.Instance {
	if tok == "good" {
		return &domain.Instance{ID: "inst-1", AgentID: "stock_daemon"}
	}
	return nil
}
func (tokenManager) GetInstanceIDs(string) []string { return nil }
func (tokenManager) EvictInstance(string)           {}

type countingPlanner struct{ calls int32 }

func (p *countingPlanner) GetExecutionPlan(context.Context, string) (*domain.ExecutionPlan, error) {
	atomic.AddInt32(&p.calls, 1)
	return &domain.ExecutionPlan{Subject: "restock", Steps: []domain.Step{{Query: "order GPUs"}}}, nil
}

type chanPlanExecutor struct{ ran chan struct{} }

func (e chanPlanExecutor) Execute(context.Context, *domain.ExecutionPlan, *domain.Handoff) (*domain.Handoff, error) {
	e.ran <- struct{}{}
	return &domain.Handoff{}, nil
}

// In the OSS wiring the Watcher is both the server's gate and its
// SignalReceiver. Each valid signal must be planned exactly ONCE, and the plan
// must reach the operator approval hub and run on approval (ticket
// T-20261031-oss-signalstream-plans-each-signal-twice-and). Before the fix the
// gate called ProcessSignal and OnSignal planned again: 2 planner calls, both
// plans discarded.
func TestSignalStream_OSSWatcherPlansOnceAndQueuesForApproval(t *testing.T) {
	t.Parallel()
	planner := &countingPlanner{}
	hub := domain.NewInMemoryApprovalController(5 * time.Second)
	reqs, cancel := hub.Watch()
	defer cancel()
	exec := chanPlanExecutor{ran: make(chan struct{}, 1)}
	w := supwatcher.New(tokenManager{}, nil, planner, hub, exec, supwatcher.WatcherConfig{})
	s := &Server{Watcher: w, SignalReceiver: w}

	ctx := metadata.NewIncomingContext(t.Context(), metadata.Pairs("authorization", "Bearer good"))
	stream := &fakeSignalStream{ctx: ctx, in: []*pb.Handoff{{
		Id: "sig-1", FromAgent: "stock_daemon",
		Payload:  &pb.Object{Data: []byte("GPU:5")},
		Metadata: map[string]string{"_signal_type": "STOCK_LOW"},
	}}}
	if err := s.SignalStream(stream); err != io.EOF {
		t.Fatalf("SignalStream: %v", err)
	}
	if got := atomic.LoadInt32(&planner.calls); got != 1 {
		t.Fatalf("planner calls per signal = %d, want 1", got)
	}

	var req domain.ApprovalRequest
	select {
	case req = <-reqs:
	case <-time.After(2 * time.Second):
		t.Fatal("plan was not queued for operator approval")
	}
	hub.Submit(req.ID, true, "operator")
	select {
	case <-exec.ran:
	case <-time.After(2 * time.Second):
		t.Fatal("approved plan was not executed")
	}
}

// A signal the gate rejects (bad token) never reaches the planner.
func TestSignalStream_RejectedTokenIsNotPlanned(t *testing.T) {
	t.Parallel()
	planner := &countingPlanner{}
	w := supwatcher.New(tokenManager{}, nil, planner, nil, nil, supwatcher.WatcherConfig{})
	s := &Server{Watcher: w, SignalReceiver: w}

	ctx := metadata.NewIncomingContext(t.Context(), metadata.Pairs("authorization", "Bearer bad"))
	stream := &fakeSignalStream{ctx: ctx, in: []*pb.Handoff{{
		Id: "sig-1", FromAgent: "x", Metadata: map[string]string{"_signal_type": "S"},
	}}}
	_ = s.SignalStream(stream)
	if got := atomic.LoadInt32(&planner.calls); got != 0 {
		t.Fatalf("planner calls = %d, want 0 for a rejected signal", got)
	}
}
