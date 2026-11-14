package network

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ENG-1377 (owner decision 2026-11-14): an authored plan (SubmitPlan,
// KernelServices.PlanExecutor) under the single-agent lane is handed to the lane's
// agent as its brief; never an empty dispatch. Under the planner lane it still
// runs step by step on the DAG executor.

func authoredPlanJSON(t *testing.T, p domain.ExecutionPlan) string {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func twoStepPlan() domain.ExecutionPlan {
	return domain.ExecutionPlan{Subject: "Quarterly supplier review", Steps: []domain.Step{
		{Query: "list the suppliers with late deliveries this quarter"},
		{Query: "draft a reminder to each of them", DependsOn: []int{0}, CheckpointAfter: true},
	}}
}

func TestAuthoredPlanBrief_RendersStepsInOrder(t *testing.T) {
	fan := 0
	p := twoStepPlan()
	p.Steps = append(p.Steps, domain.Step{Query: "file {supplier}", FanOutOver: &fan, FanOutVar: "supplier"},
		domain.Step{Query: "weigh the options", IsThought: true})
	brief, err := authoredPlanBrief(authoredPlanJSON(t, p), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Plan: Quarterly supplier review",
		"1. list the suppliers with late deliveries this quarter\n",
		"2. draft a reminder to each of them (after step 1)",
		"3. file {supplier} (once for each item step 1 produced, as {supplier})",
		"4. weigh the options (reasoning only, no tools)",
		"run_subtasks",
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief lacks %q:\n%s", want, brief)
		}
	}
	if strings.Index(brief, "1. list") > strings.Index(brief, "2. draft") {
		t.Errorf("steps out of order:\n%s", brief)
	}
}

func TestAuthoredPlanBrief_CallerInputLeadsUnlessItIsTheSubject(t *testing.T) {
	js := authoredPlanJSON(t, twoStepPlan())
	b, _ := authoredPlanBrief(js, "please focus on EU suppliers")
	if !strings.HasPrefix(b, "please focus on EU suppliers\n\n") {
		t.Errorf("caller input does not lead the brief:\n%s", b)
	}
	// PlanExecutor sends the subject as the payload when its caller gave none:
	// not repeated.
	b, _ = authoredPlanBrief(js, "Quarterly supplier review")
	if strings.Count(b, "Quarterly supplier review") != 1 {
		t.Errorf("subject repeated:\n%s", b)
	}
}

func TestAuthoredPlanBrief_RefusesEmptyInvalidAndCyclicPlans(t *testing.T) {
	for name, js := range map[string]string{
		"no steps": authoredPlanJSON(t, domain.ExecutionPlan{Subject: "x"}),
		"not json": "{",
		"cyclic":   authoredPlanJSON(t, domain.ExecutionPlan{Steps: []domain.Step{{Query: "a", DependsOn: []int{1}}, {Query: "b", DependsOn: []int{0}}}}),
	} {
		if _, err := authoredPlanBrief(js, ""); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: err = %v, want InvalidArgument", name, err)
		}
	}
}

func singleAgentServer(fake *fakeAuctioneer) *Server {
	return &Server{
		Dispatcher: fake,
		ExecCfg: config.ExecutionConfig{Lane: config.LaneConfig{
			Default:     config.LaneSingleAgent,
			SingleAgent: config.SingleAgentLaneConfig{AgentID: "lane_agent"},
		}},
	}
}

// SubmitPlan's shape: an empty payload, the plan only in metadata. The agent gets
// the rendered plan, not "".
func TestExecute_SingleAgentLane_AuthoredPlanIsTheAgentsBrief(t *testing.T) {
	t.Parallel()
	fake := &fakeAuctioneer{resp: &domain.Handoff{Payload: &domain.Payload{Data: []byte("done")}}}
	s := singleAgentServer(fake)
	_, err := s.Execute(context.Background(), &pb.Handoff{
		Payload:  &pb.Object{Data: []byte("")},
		Metadata: map[string]string{AuthoredPlanMetadataKey: authoredPlanJSON(t, twoStepPlan())},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if fake.calledAgent != "lane_agent" || fake.gotHandoff == nil {
		t.Fatalf("not dispatched to the lane agent (got %q)", fake.calledAgent)
	}
	got := string(fake.gotHandoff.Payload.Data)
	if !strings.Contains(got, "1. list the suppliers with late deliveries this quarter") ||
		!strings.Contains(got, "2. draft a reminder to each of them (after step 1)") {
		t.Fatalf("agent brief does not carry the plan:\n%q", got)
	}
	if fake.gotHandoff.Context["original_prompt"] != got {
		t.Errorf("original_prompt = %q, want the brief", fake.gotHandoff.Context["original_prompt"])
	}
}

func TestExecute_SingleAgentLane_EmptyAuthoredPlanIsRefusedNotDispatched(t *testing.T) {
	t.Parallel()
	fake := &fakeAuctioneer{resp: &domain.Handoff{Payload: &domain.Payload{Data: []byte("done")}}}
	s := singleAgentServer(fake)
	_, err := s.Execute(context.Background(), &pb.Handoff{
		Payload:  &pb.Object{Data: []byte("")},
		Metadata: map[string]string{AuthoredPlanMetadataKey: authoredPlanJSON(t, domain.ExecutionPlan{Subject: "nothing"})},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("err = %v, want InvalidArgument", err)
	}
	if fake.calledAgent != "" {
		t.Fatalf("an empty plan was dispatched to %q", fake.calledAgent)
	}
}

// stepRecorder records each dispatched step's description.
type stepRecorder struct {
	mu    sync.Mutex
	steps []string
}

func (r *stepRecorder) Execute(_ context.Context, task *domain.DispatchTask, _ *domain.Handoff) (*domain.DispatchResult, error) {
	r.mu.Lock()
	r.steps = append(r.steps, task.Description)
	r.mu.Unlock()
	return &domain.DispatchResult{Handoff: &domain.Handoff{FromAgent: "worker", Payload: &domain.Payload{Data: []byte("ok")}}, Confidence: 0.9}, nil
}

func (r *stepRecorder) CallAgent(context.Context, string, *domain.Handoff, string) (*domain.Handoff, error) {
	return nil, nil
}

// The planner lane is unchanged: the authored plan's steps run one by one and
// the planner is never asked.
func TestExecute_PlannerLane_AuthoredPlanRunsItsSteps(t *testing.T) {
	t.Parallel()
	s := minimalServer(t)
	s.ExecCfg.Lane.Default = config.LanePlanner
	rec := &stepRecorder{}
	s.Dispatcher = rec
	_, err := s.Execute(context.Background(), &pb.Handoff{
		Id:       "authored",
		Payload:  &pb.Object{Data: []byte("")},
		Metadata: map[string]string{AuthoredPlanMetadataKey: authoredPlanJSON(t, twoStepPlan())},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.steps) != 2 || !strings.Contains(rec.steps[0], "list the suppliers") || !strings.Contains(rec.steps[1], "draft a reminder") {
		t.Fatalf("dispatched steps = %q, want the plan's two steps in order", rec.steps)
	}
	if mp := s.Planner.(*mockPlanner); len(mp.calls) != 0 {
		t.Fatalf("the planner was asked (%d calls) for an authored plan", len(mp.calls))
	}
}
