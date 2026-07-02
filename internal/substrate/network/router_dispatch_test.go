package network

import (
	"context"
	"encoding/json"
	"testing"

	pb "github.com/cambrian-sh/cambrian-runtime/api/proto"
	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// ── Stub Router ───────────────────────────────────────────────────────────────

// stubRouter returns a fixed decision for every Resolve call.
type stubRouter struct {
	decision *domain.RouterDecision
	err      error
}

func (r *stubRouter) Resolve(_ context.Context, _ domain.RouterInput) (*domain.RouterDecision, error) {
	return r.decision, r.err
}

func routerReturning(t domain.DecisionType) *stubRouter {
	return &stubRouter{decision: &domain.RouterDecision{Type: t}}
}

func routerReturningClarification() *stubRouter {
	return &stubRouter{decision: &domain.RouterDecision{
		Type:                  domain.DecisionClarification,
		ClarificationQuestion: "What would you like me to do?",
		ClarificationOptions: []domain.ClarificationOption{
			{Label: "Build a plan", Decision: domain.DecisionPlan, Recommended: true},
			{Label: "Answer from memory", Decision: domain.DecisionChat},
		},
	}}
}

// ── Stub Planner ──────────────────────────────────────────────────────────────

type stubPlanner struct{}

func (p *stubPlanner) GetExecutionPlan(_ context.Context, _ string) (*domain.ExecutionPlan, error) {
	return &domain.ExecutionPlan{
		Subject: "stub",
		Steps:   []domain.Step{{Query: "stub step"}},
	}, nil
}

func (p *stubPlanner) Generate(_ context.Context, _ string) (string, error) {
	return "", nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func serverWithRouter(r domain.InputRouter) *Server {
	s := &Server{
		Router:  r,
		Planner: &stubPlanner{},
		ExecCfg: tokenTestExecCfg(),
		Auctioneer: &capturingAuctioneer{},
	}
	return s
}

func handoffWithBody(body string) *pb.Handoff {
	return &pb.Handoff{
		Payload: &pb.Object{Data: []byte(body), Type: "text/plain"},
	}
}

// ── Tests ────────────────────────────────────────────────────────────────────

// Cycle 38 — CHAT decision returns payload.type = "not_implemented".
func TestServer_RouterChat_ReturnsNotImplemented(t *testing.T) {
	s := serverWithRouter(routerReturning(domain.DecisionChat))
	resp, err := s.Execute(context.Background(), handoffWithBody("what is France?"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if string(resp.Payload.Type) != "not_implemented" {
		t.Fatalf("expected payload.type='not_implemented', got %q", resp.Payload.Type)
	}
}

// Cycle 39 — WATCH decision returns payload.type = "not_implemented".
func TestServer_RouterWatch_ReturnsNotImplemented(t *testing.T) {
	s := serverWithRouter(routerReturning(domain.DecisionWatch))
	resp, err := s.Execute(context.Background(), handoffWithBody("watch gold prices"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.Payload.Type != "not_implemented" {
		t.Fatalf("expected payload.type='not_implemented', got %q", resp.Payload.Type)
	}
}

// Cycle 40 — CLARIFICATION decision returns payload.type = "clarification"
// with valid JSON containing question and options.
func TestServer_RouterClarification_ReturnsStructuredJSON(t *testing.T) {
	s := serverWithRouter(routerReturningClarification())
	resp, err := s.Execute(context.Background(), handoffWithBody("help me with this"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.Payload.Type != "clarification" {
		t.Fatalf("expected payload.type='clarification', got %q", resp.Payload.Type)
	}

	var body map[string]any
	if err := json.Unmarshal(resp.Payload.Data, &body); err != nil {
		t.Fatalf("payload.data is not valid JSON: %v — got: %s", err, resp.Payload.Data)
	}
	if _, ok := body["question"]; !ok {
		t.Error("expected 'question' field in clarification JSON")
	}
	opts, ok := body["options"].([]any)
	if !ok || len(opts) == 0 {
		t.Error("expected non-empty 'options' array in clarification JSON")
	}
}

// Cycle 41 — CLARIFICATION options include Recommended=true for the first option.
func TestServer_RouterClarification_RecommendedFlagPresent(t *testing.T) {
	s := serverWithRouter(routerReturningClarification())
	resp, _ := s.Execute(context.Background(), handoffWithBody("help me"))

	type option struct {
		Label       string `json:"label"`
		Decision    string `json:"decision"`
		Recommended bool   `json:"recommended"`
	}
	var body struct {
		Options []option `json:"options"`
	}
	if err := json.Unmarshal(resp.Payload.Data, &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	var hasRecommended bool
	for _, o := range body.Options {
		if o.Recommended {
			hasRecommended = true
			break
		}
	}
	if !hasRecommended {
		t.Fatal("expected at least one option with recommended=true")
	}
}

