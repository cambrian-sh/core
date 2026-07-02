package kernel

import (
	"context"
	"testing"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/infrastructure/llm"
)

// fakeCaller records the handoff Context it receives so we can assert the session
// token was injected. Satisfies domain.Auctioneer.
type fakeCaller struct {
	gotToken string
	resp     string
}

func (f *fakeCaller) Execute(context.Context, *domain.AuctionTask, *domain.Handoff) (*domain.AuctionResult, error) {
	return nil, nil
}
func (f *fakeCaller) CallAgent(_ context.Context, _ string, h *domain.Handoff, _ string) (*domain.Handoff, error) {
	if h.Context != nil {
		f.gotToken = h.Context["_session_token_id"]
	}
	return &domain.Handoff{Payload: &domain.Payload{Data: []byte(f.resp)}}, nil
}

type fakeGateway struct{ acquired, completed bool }

func (g *fakeGateway) Acquire(context.Context, domain.StepAllocation, int, time.Duration) (string, error) {
	g.acquired = true
	return "sess-test-1", nil
}
func (g *fakeGateway) Complete(context.Context, string) (llm.TokenUsage, error) {
	g.completed = true
	return llm.TokenUsage{}, nil
}

// Regression for the UNAUTHENTICATED "session_token_id is required" failure: the
// interview scenario runner must mint a managed LLM session and inject its token
// into the handoff so the agent's budgeted generate() is authorized.
func TestScenarioRunner_InjectsSessionToken(t *testing.T) {
	caller := &fakeCaller{resp: "the answer"}
	gw := &fakeGateway{}
	r := &scenarioRunner{caller: caller, gw: gw, primaryModelID: "llm:ollama:qwen3:8b"}

	answer, _, err := r.RunScenario(context.Background(), domain.AgentDefinition{ID: "a1"}, "q1", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if answer != "the answer" {
		t.Errorf("answer = %q, want %q", answer, "the answer")
	}
	if !gw.acquired || !gw.completed {
		t.Errorf("session must be minted AND completed: acquired=%v completed=%v", gw.acquired, gw.completed)
	}
	if caller.gotToken != "sess-test-1" {
		t.Errorf("_session_token_id not injected into the handoff, got %q", caller.gotToken)
	}
}

// With no gateway wired the runner still dispatches (degraded), without a token —
// it must not panic; the agent's generate would then fail and surface as a 0 score.
func TestScenarioRunner_NoGatewayNoToken(t *testing.T) {
	caller := &fakeCaller{resp: "x"}
	r := &scenarioRunner{caller: caller}
	if _, _, err := r.RunScenario(context.Background(), domain.AgentDefinition{ID: "a1"}, "q", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if caller.gotToken != "" {
		t.Errorf("expected no token without a gateway, got %q", caller.gotToken)
	}
}
