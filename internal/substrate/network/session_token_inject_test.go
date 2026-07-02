package network

import (
	"context"
	"sync"
	"testing"
	"time"

	pb "github.com/cambrian-sh/cambrian-runtime/api/proto"
	"github.com/cambrian-sh/cambrian-runtime/internal/config"
	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/infrastructure/llm"
	agentmgr "github.com/cambrian-sh/cambrian-runtime/internal/metabolism/agentmgr"
	metabolism "github.com/cambrian-sh/cambrian-runtime/internal/metabolism"
)

// capturingGateway implements LLMGateway and records Acquire/Complete calls.
type capturingGateway struct {
	mu            sync.Mutex
	acquireCount  int
	completeCount int
	tokenToReturn string
}

func (g *capturingGateway) Acquire(_ context.Context, _ domain.StepAllocation, _ int, _ time.Duration) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.acquireCount++
	if g.tokenToReturn == "" {
		return "test-token", nil
	}
	return g.tokenToReturn, nil
}

func (g *capturingGateway) Complete(_ context.Context, _ string) (llm.TokenUsage, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.completeCount++
	return llm.TokenUsage{}, nil
}

func (g *capturingGateway) StreamChunks(_ context.Context, _ string, _ string, _ domain.GenerateOptions, _ chan<- domain.StreamChunk) error {
	return nil
}

func (g *capturingGateway) EvictExpired() {}

// capturingAuctioneer records the context of each handoff it receives.
type capturingAuctioneer struct {
	mu       sync.Mutex
	received []*domain.Handoff
}

func (a *capturingAuctioneer) Execute(ctx context.Context, task *domain.AuctionTask, h *domain.Handoff) (*domain.AuctionResult, error) {
	a.mu.Lock()
	clone := &domain.Handoff{Context: make(map[string]string)}
	for k, v := range h.Context {
		clone.Context[k] = v
	}
	a.received = append(a.received, clone)
	a.mu.Unlock()
	return &domain.AuctionResult{
		Handoff:    &domain.Handoff{Payload: &domain.Payload{Data: []byte("ok")}},
		Confidence: 0.9,
	}, nil
}

func (a *capturingAuctioneer) CallAgent(ctx context.Context, agentID string, h *domain.Handoff, excludeInstanceID string) (*domain.Handoff, error) {
	return &domain.Handoff{Payload: &domain.Payload{Data: []byte("ok")}}, nil
}


func tokenTestExecCfg() config.ExecutionConfig {
	return config.ExecutionConfig{
		PlanTimeoutMs:     30000,
		StepTimeoutBaseBufferMs: 5000,
		StepTimeoutMultiplier: 2.0,
	}
}

// minimalServer returns a Server wired with just enough to run a one-step plan.
func minimalServer(t *testing.T) *Server {
	t.Helper()
	plan := &domain.ExecutionPlan{Subject: "test", Steps: []domain.Step{{Query: "do it"}}}
	planner := &mockPlanner{responses: []plannerResponse{{plan: plan}}}
	reg := metabolism.NewInMemoryRegistry()
	mgr := agentmgr.NewAgentManager(reg, "python", "unix:///tmp/test.sock", nil)
	return &Server{
		Planner: planner,
		Manager: mgr,
		ExecCfg: tokenTestExecCfg(),
	}
}

// runOneStepPlan drives s.Execute with a one-handoff request.
func runOneStepPlan(t *testing.T, s *Server) {
	t.Helper()
	_, _ = s.Execute(context.Background(), &pb.Handoff{
		Id:      "test-id",
		Payload: &pb.Object{Data: []byte("do it")},
	})
}

func TestExecute_InjectsSessionTokenID(t *testing.T) {
	gw := &capturingGateway{tokenToReturn: "tok-xyz"}
	auc := &capturingAuctioneer{}
	s := minimalServer(t)
	s.LLMGateway = gw
	s.Auctioneer = auc

	runOneStepPlan(t, s)

	auc.mu.Lock()
	defer auc.mu.Unlock()
	if len(auc.received) == 0 {
		t.Fatal("auctioneer never called")
	}
	if got := auc.received[0].Context["_session_token_id"]; got != "tok-xyz" {
		t.Errorf("expected _session_token_id=tok-xyz, got %q", got)
	}
}

func TestExecute_InjectsStepIndex(t *testing.T) {
	auc := &capturingAuctioneer{}
	s := minimalServer(t)
	s.Auctioneer = auc

	runOneStepPlan(t, s)

	auc.mu.Lock()
	defer auc.mu.Unlock()
	if len(auc.received) == 0 {
		t.Fatal("auctioneer never called")
	}
	if got := auc.received[0].Context["_step_index"]; got != "0" {
		t.Errorf("expected _step_index=0, got %q", got)
	}
}

func TestExecute_NilGateway_NoTokenInjected(t *testing.T) {
	auc := &capturingAuctioneer{}
	s := minimalServer(t)
	s.LLMGateway = nil
	s.Auctioneer = auc

	runOneStepPlan(t, s)

	auc.mu.Lock()
	defer auc.mu.Unlock()
	if len(auc.received) == 0 {
		t.Fatal("auctioneer never called")
	}
	if _, ok := auc.received[0].Context["_session_token_id"]; ok {
		t.Error("expected no _session_token_id when LLMGateway is nil")
	}
}

func TestExecute_GatewayCompleteCalledAfterStep(t *testing.T) {
	gw := &capturingGateway{}
	auc := &capturingAuctioneer{}
	s := minimalServer(t)
	s.LLMGateway = gw
	s.Auctioneer = auc

	runOneStepPlan(t, s)

	gw.mu.Lock()
	defer gw.mu.Unlock()
	if gw.acquireCount != 1 {
		t.Errorf("expected Acquire called once, got %d", gw.acquireCount)
	}
	if gw.completeCount != 1 {
		t.Errorf("expected Complete called once (deferred), got %d", gw.completeCount)
	}
}
