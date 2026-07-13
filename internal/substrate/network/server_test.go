package network

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/metabolism"
	"github.com/cambrian-sh/core/internal/metabolism/agentmgr"
	metauc "github.com/cambrian-sh/core/internal/metabolism/auctioneer"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
	supgk "github.com/cambrian-sh/core/internal/supervision/gatekeeper"
)

// mockPlanner records every prompt it receives and returns preset plans in order.
type mockPlanner struct {
	responses []plannerResponse
	calls     []string // prompts received
	genResp   string   // response for Generate
	genErr    error
	genCalls  []string // prompts received by Generate
}

type plannerResponse struct {
	plan *domain.ExecutionPlan
	err  error
}

// step is a helper used by DAGExecutor-related tests in this file.
func step(deps ...int) domain.Step {
	return domain.Step{Query: "q", DependsOn: deps}
}

func (m *mockPlanner) GetExecutionPlan(_ context.Context, userInput string) (*domain.ExecutionPlan, error) {
	m.calls = append(m.calls, userInput)
	idx := len(m.calls) - 1
	if idx >= len(m.responses) {
		panic("mockPlanner: more calls than preset responses")
	}
	r := m.responses[idx]
	return r.plan, r.err
}

func (m *mockPlanner) Generate(_ context.Context, prompt string) (string, error) {
	m.genCalls = append(m.genCalls, prompt)
	return m.genResp, m.genErr
}

func TestServer_ThoughtFn(t *testing.T) {
	mock := &mockPlanner{
		genResp: "The final answer is 42.",
	}
	s := &Server{Planner: mock}

	plan := &domain.ExecutionPlan{
		Subject: "Test Subject",
		Steps: []domain.Step{
			{Query: "Summarize findings", IsThought: true},
		},
	}

	handoff := &domain.Handoff{
		Context: map[string]string{
			"step_0_result": "Agent found A.",
			"step_1_result": "Agent found B.",
		},
	}

	tf := s.thoughtFn(plan)
	resp, err := tf(t.Context(), 0, handoff)

	if err != nil {
		t.Fatalf("thoughtFn failed: %v", err)
	}

	if string(resp.Payload.Data) != "The final answer is 42." {
		t.Errorf("unexpected synthesis result: %q", string(resp.Payload.Data))
	}

	if len(mock.genCalls) != 1 {
		t.Fatalf("expected 1 Generate call, got %d", len(mock.genCalls))
	}

	prompt := mock.genCalls[0]
	if !strings.Contains(prompt, "Summarize findings") {
		t.Errorf("prompt missing step query, got: %q", prompt)
	}
	if !strings.Contains(prompt, "Agent found A.") || !strings.Contains(prompt, "Agent found B.") {
		t.Errorf("prompt missing context results, got: %q", prompt)
	}
	if !strings.Contains(prompt, "Test Subject") {
		t.Errorf("prompt missing subject, got: %q", prompt)
	}
}

// cyclicPlan returns a two-step plan where step 0 depends on step 1 and vice versa.
func cyclicPlan() *domain.ExecutionPlan {
	return &domain.ExecutionPlan{
		Subject: "test",
		Steps: []domain.Step{
			{Query: "q1", DependsOn: []int{1}},
			{Query: "q2", DependsOn: []int{0}},
		},
	}
}

// validPlan returns a two-step linear plan with no cycle.
func validPlan() *domain.ExecutionPlan {
	return &domain.ExecutionPlan{
		Subject: "test",
		Steps: []domain.Step{
			{Query: "q1"},
			{Query: "q2", DependsOn: []int{0}},
		},
	}
}

func TestPlanWithValidation_ValidPlanOnFirstCall(t *testing.T) {
	mock := &mockPlanner{responses: []plannerResponse{{plan: validPlan()}}}

	plan, err := planWithValidation(t.Context(), mock, "do something", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mock.calls) != 1 {
		t.Errorf("expected 1 planner call, got %d", len(mock.calls))
	}
	if len(plan.Steps) != 2 {
		t.Errorf("expected 2 steps, got %d", len(plan.Steps))
	}
}

func TestPlanWithValidation_CycleTriggersRetry(t *testing.T) {
	mock := &mockPlanner{responses: []plannerResponse{
		{plan: cyclicPlan()},
		{plan: validPlan()},
	}}

	plan, err := planWithValidation(t.Context(), mock, "do something", nil)
	if err != nil {
		t.Fatalf("expected success after retry, got: %v", err)
	}
	if len(mock.calls) != 2 {
		t.Errorf("expected exactly 2 planner calls, got %d", len(mock.calls))
	}
	if len(plan.Steps) != 2 {
		t.Errorf("expected 2 steps in valid plan, got %d", len(plan.Steps))
	}
}

func TestPlanWithValidation_RetryPromptContainsCycleDescription(t *testing.T) {
	mock := &mockPlanner{responses: []plannerResponse{
		{plan: cyclicPlan()},
		{plan: validPlan()},
	}}

	planWithValidation(t.Context(), mock, "original request", nil) //nolint:errcheck

	retryPrompt := mock.calls[1]
	if !strings.Contains(retryPrompt, "PREVIOUS PLAN ERROR") {
		t.Errorf("retry prompt must contain PREVIOUS PLAN ERROR marker, got: %q", retryPrompt)
	}
	if !strings.Contains(retryPrompt, "cycle") {
		t.Errorf("retry prompt must contain cycle description, got: %q", retryPrompt)
	}
	if !strings.Contains(retryPrompt, "original request") {
		t.Errorf("retry prompt must preserve the original user input, got: %q", retryPrompt)
	}
}

func TestPlanWithValidation_HardFailAfterTwoCyclicPlans(t *testing.T) {
	mock := &mockPlanner{responses: []plannerResponse{
		{plan: cyclicPlan()},
		{plan: cyclicPlan()},
	}}

	_, err := planWithValidation(t.Context(), mock, "do something", nil)
	if err == nil {
		t.Fatal("expected hard error after two cyclic plans, got nil")
	}
	if len(mock.calls) != 2 {
		t.Errorf("expected exactly 2 planner calls before hard fail, got %d", len(mock.calls))
	}
}

// ============================================================
// stepTimeout formula tests
// ============================================================

func TestStepTimeout_Formula(t *testing.T) {
	cases := []struct {
		latency  int
		mult     float64
		base     int
		wantMs   int
	}{
		{100, 2.0, 5000, 5200}, // 100*2 + 5000
		{500, 1.5, 1000, 1750}, // 500*1.5 + 1000
		{200, 3.0, 0, 600},     // 200*3 + 0
	}
	for _, c := range cases {
		got := stepTimeout(c.latency, c.mult, c.base)
		want := time.Duration(c.wantMs) * time.Millisecond
		if got != want {
			t.Errorf("stepTimeout(%d, %.1f, %d) = %v, want %v", c.latency, c.mult, c.base, got, want)
		}
	}
}

func TestStepTimeout_ZeroLatency_DegradestoBaseBuffer(t *testing.T) {
	got := stepTimeout(0, 2.0, 5000)
	want := 5000 * time.Millisecond
	if got != want {
		t.Errorf("zero latency: got %v, want %v", got, want)
	}
}

// ============================================================
// DAGExecutor tests
// ============================================================

func TestDAGExecutor_PerStepTimeout_CancelsSlowStep(t *testing.T) {
	stepFn := func(ctx context.Context, _ int, _ *domain.Handoff) (*domain.Handoff, error) {
		timeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
		defer cancel()
		select {
		case <-time.After(500 * time.Millisecond):
			return &domain.Handoff{Payload: &domain.Payload{Data: []byte("ok")}}, nil
		case <-timeoutCtx.Done():
			return nil, timeoutCtx.Err()
		}
	}

	plan := &domain.ExecutionPlan{Steps: []domain.Step{step()}}
	_, err := (&executer.DAGExecutor{}).Execute(t.Context(), plan, nil, executer.StepFunc(stepFn))
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected DeadlineExceeded, got: %v", err)
	}
}

func TestDAGExecutor_PlanLevelTimeout_CancelsInFlightSteps(t *testing.T) {
	stepFn := func(ctx context.Context, _ int, _ *domain.Handoff) (*domain.Handoff, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	plan := &domain.ExecutionPlan{Steps: []domain.Step{step(), step()}}

	planCtx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	_, err := (&executer.DAGExecutor{}).Execute(planCtx, plan, nil, executer.StepFunc(stepFn))
	if err == nil {
		t.Fatal("expected plan-level timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected DeadlineExceeded from plan timeout, got: %v", err)
	}
}

// ============================================================
// PartialPlanError repack detection
// ============================================================

func TestPartialPlanError_RepackPattern(t *testing.T) {
	partialCtx := map[string]string{
		"step_0_result": "result_A",
		"step_1_result": "result_B",
	}
	partialErr := &executer.PartialPlanError{
		FailedStep:  2,
		LastError:   errors.New("step 2 failed"),
		Context:     partialCtx,
		ReplanCount: 0,
	}

	var detected *executer.PartialPlanError
	if !errors.As(partialErr, &detected) {
		t.Fatal("errors.As should detect PartialPlanError")
	}

	detected.Context["_partial_plan"] = "true"
	handoff := &domain.Handoff{
		FromAgent: "orchestrator",
		ToAgent:   "user",
		Payload:   &domain.Payload{Data: []byte(detected.Error())},
		Context:   detected.Context,
	}

	if handoff.Context["_partial_plan"] != "true" {
		t.Error("_partial_plan key not set to true")
	}
	if handoff.Context["step_0_result"] != "result_A" {
		t.Error("step_0_result missing from handoff context")
	}
	if handoff.Context["step_1_result"] != "result_B" {
		t.Error("step_1_result missing from handoff context")
	}
	if !strings.Contains(string(handoff.Payload.Data), "step 2") {
		t.Errorf("payload should contain error details, got: %q", string(handoff.Payload.Data))
	}
}

func TestPartialPlanError_RepackHandlesEmptyContext(t *testing.T) {
	partialErr := &executer.PartialPlanError{
		FailedStep: 0,
		LastError:  errors.New("startup failed"),
		Context:    map[string]string{},
	}

	var detected *executer.PartialPlanError
	if !errors.As(partialErr, &detected) {
		t.Fatal("errors.As should detect PartialPlanError")
	}

	detected.Context["_partial_plan"] = "true"
	handoff := &domain.Handoff{
		FromAgent: "orchestrator",
		ToAgent:   "user",
		Payload:   &domain.Payload{Data: []byte(detected.Error())},
		Context:   detected.Context,
	}

	if handoff.Context["_partial_plan"] != "true" {
		t.Error("_partial_plan should be set even with empty context")
	}
	if len(handoff.Context) != 1 {
		t.Errorf("expected exactly 1 context entry (_partial_plan), got %d: %v", len(handoff.Context), handoff.Context)
	}
}

// ============================================================
// Inter-step fallback tests
// ============================================================

func TestServer_StepFn_FallbackUsesRunnerUpWhenWinnerFails(t *testing.T) {
	reg := metabolism.NewInMemoryRegistry()
	reg.SetAgent(domain.AgentDefinition{
		ID:      "winner",
		Name:    "winner",
		Runtime: domain.RuntimePython,
		Trait:   domain.TraitTool,
	})
	reg.SetAgent(domain.AgentDefinition{
		ID:      "runner-up-a",
		Name:    "runner-up-a",
		Runtime: domain.RuntimeWasm,
		Trait:   "",
	})

	mgr := agentmgr.NewAgentManager(reg, "python", "unix://tmp/cambrian.sock", nil)
	gk := supgk.NewGatekeeper(reg, config.ExecutionConfig{
		GatekeeperMaxCandidates: 5,
		GatekeeperW1:            0.4,
		GatekeeperW2:            0.4,
		GatekeeperW3:            0.2,
	})
	auctioneer := metauc.New(mgr, gk, config.ExecutionConfig{
		FallbackEnabled:             true,
		FallbackConfidenceThreshold: 0.4,
	})

	auctioneer.RequestProposalHook = func(ctx context.Context, agent domain.AgentDefinition, task *domain.AuctionTask, confidenceHint float32) (*domain.AgentProposal, error) {
		conf := 0.5
		if agent.ID == "winner" {
			conf = 0.9
		}
		return &domain.AgentProposal{
			AgentID:    agent.ID,
			TaskID:     task.ID,
			Confidence: float64(conf),
		}, nil
	}

	var callAgentCalls []string
	auctioneer.CallAgentHook = func(ctx context.Context, agentID string, handoff *domain.Handoff, excludeInstanceID string) (*domain.Handoff, error) {
		callAgentCalls = append(callAgentCalls, agentID)
		if agentID == "runner-up-a" {
			return &domain.Handoff{
				Payload:   &domain.Payload{Data: []byte("fallback result")},
				FromAgent: "runner-up-a",
				Context: map[string]string{
					"_thought_trace": "fallback trace",
				},
			}, nil
		}
		return nil, errors.New("simulated agent failure")
	}

	mockPlan := &mockPlanner{
		responses: []plannerResponse{{plan: &domain.ExecutionPlan{
			Subject: "test",
			Steps:   []domain.Step{{Query: "do thing"}},
		}}},
	}

	s := &Server{
		Planner:    mockPlan,
		Manager:    mgr,
		Auctioneer: auctioneer,
		ExecCfg: config.ExecutionConfig{
			FallbackEnabled:             true,
			FallbackConfidenceThreshold: 0.4,
			PlanTimeoutMs:               5000,
		},
	}

	resp, err := s.Execute(t.Context(), &pb.Handoff{
		Payload: &pb.Object{Data: []byte("test")},
	})

	if err != nil {
		t.Fatalf("Execute should succeed via fallback, got: %v", err)
	}

	winnerCalls := 0
	for _, c := range callAgentCalls {
		if c == "winner" {
			winnerCalls++
		}
	}
	if winnerCalls != 2 {
		t.Errorf("expected 2 SelfHealer attempts for winner before loop detected, got %d (calls: %v)", winnerCalls, callAgentCalls)
	}

	foundFallback := false
	for _, c := range callAgentCalls {
		if c == "runner-up-a" {
			foundFallback = true
			break
		}
	}
	if !foundFallback {
		t.Errorf("fallback never tried runner-up-a, calls: %v", callAgentCalls)
	}

	if string(resp.Payload.Data) != "fallback result" {
		t.Errorf("expected fallback result, got %q", string(resp.Payload.Data))
	}
}

func TestServer_StepFn_FallbackPropagatesErrorWhenAllRunnerUpsFail(t *testing.T) {
	reg := metabolism.NewInMemoryRegistry()
	reg.SetAgent(domain.AgentDefinition{
		ID:      "winner",
		Name:    "winner",
		Runtime: domain.RuntimePython,
		Trait:   domain.TraitTool,
	})
	reg.SetAgent(domain.AgentDefinition{
		ID:      "runner-up-a",
		Name:    "runner-up-a",
		Runtime: domain.RuntimeWasm,
		Trait:   "",
	})

	mgr := agentmgr.NewAgentManager(reg, "python", "unix://tmp/cambrian.sock", nil)
	gk := supgk.NewGatekeeper(reg, config.ExecutionConfig{
		GatekeeperMaxCandidates: 5,
		GatekeeperW1:            0.4,
		GatekeeperW2:            0.4,
		GatekeeperW3:            0.2,
	})
	auctioneer := metauc.New(mgr, gk, config.ExecutionConfig{
		FallbackEnabled:             true,
		FallbackConfidenceThreshold: 0.4,
	})

	auctioneer.RequestProposalHook = func(ctx context.Context, agent domain.AgentDefinition, task *domain.AuctionTask, confidenceHint float32) (*domain.AgentProposal, error) {
		conf := 0.5
		if agent.ID == "winner" {
			conf = 0.9
		}
		return &domain.AgentProposal{
			AgentID:    agent.ID,
			TaskID:     task.ID,
			Confidence: float64(conf),
		}, nil
	}

	var callAgentCalls []string
	auctioneer.CallAgentHook = func(ctx context.Context, agentID string, handoff *domain.Handoff, excludeInstanceID string) (*domain.Handoff, error) {
		callAgentCalls = append(callAgentCalls, agentID)
		return nil, errors.New("simulated failure for " + agentID)
	}

	mockPlan := &mockPlanner{
		responses: []plannerResponse{{plan: &domain.ExecutionPlan{
			Subject: "test",
			Steps:   []domain.Step{{Query: "do thing"}},
		}}},
	}

	s := &Server{
		Planner:    mockPlan,
		Manager:    mgr,
		Auctioneer: auctioneer,
		ExecCfg: config.ExecutionConfig{
			FallbackEnabled:             true,
			FallbackConfidenceThreshold: 0.4,
			PlanTimeoutMs:               5000,
		},
	}

	resp, err := s.Execute(t.Context(), &pb.Handoff{
		Payload: &pb.Object{Data: []byte("test")},
	})

	if err != nil {
		t.Fatalf("Execute should return partial-plan handoff, got error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response for partial plan")
	}
	if resp.Metadata["_partial_plan"] != "true" {
		t.Errorf("expected _partial_plan=true in response context, got: %v", resp.Metadata)
	}

	winnerCalls := 0
	for _, c := range callAgentCalls {
		if c == "winner" {
			winnerCalls++
		}
	}
	if winnerCalls != 2 {
		t.Errorf("expected 2 SelfHealer attempts for winner, got %d", winnerCalls)
	}

	foundFallback := false
	for _, c := range callAgentCalls {
		if c == "runner-up-a" {
			foundFallback = true
			break
		}
	}
	if !foundFallback {
		t.Errorf("fallback should attempt runner-up-a, calls: %v", callAgentCalls)
	}
}
