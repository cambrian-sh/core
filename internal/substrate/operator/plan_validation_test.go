package operator

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

func issueWithCode(issues []PlanIssue, code string) (PlanIssue, bool) {
	for _, i := range issues {
		if i.Code == code {
			return i, true
		}
	}
	return PlanIssue{}, false
}

func allAgentsKnown(string) bool { return true }
func noAgentsKnown(string) bool  { return false }

// The reason SubmitPlan needs validation at all: the planner path cannot emit a
// cycle, and an operator drawing a DAG by hand can.
func TestValidateAuthoredPlan_CycleIsFatal(t *testing.T) {
	steps := []domain.Step{
		{Query: "a", DependsOn: []int{1}},
		{Query: "b", DependsOn: []int{0}},
	}
	issues, order := validateAuthoredPlan(steps, allAgentsKnown)

	i, ok := issueWithCode(issues, IssueCycle)
	if !ok {
		t.Fatalf("no cycle issue in %+v", issues)
	}
	if !i.Fatal {
		t.Fatal("a cycle must be fatal — the executor cannot run this plan at all")
	}
	if order != nil {
		t.Fatalf("execution order = %v, want nil for an unrunnable plan", order)
	}
}

func TestValidateAuthoredPlan_SelfDependencyIsFatal(t *testing.T) {
	steps := []domain.Step{{Query: "a", DependsOn: []int{0}}}
	issues, _ := validateAuthoredPlan(steps, allAgentsKnown)

	if _, ok := issueWithCode(issues, IssueCycle); !ok {
		t.Fatalf("no cycle issue for a self-dependency: %+v", issues)
	}
}

func TestValidateAuthoredPlan_OutOfBoundsDependencyIsFatal(t *testing.T) {
	steps := []domain.Step{{Query: "a", DependsOn: []int{7}}}
	issues, _ := validateAuthoredPlan(steps, allAgentsKnown)

	i, ok := issueWithCode(issues, IssueOutOfBoundsDep)
	if !ok {
		t.Fatalf("no out-of-bounds issue in %+v", issues)
	}
	if !i.Fatal {
		t.Fatal("an out-of-bounds dependency must be fatal")
	}
}

// A valid plan returns the order the executor would actually use, which is the
// question a DAG author is asking — a drawing does not answer it past a few nodes.
func TestValidateAuthoredPlan_ValidPlanReturnsExecutionOrder(t *testing.T) {
	steps := []domain.Step{
		{Query: "c", DependsOn: []int{1}},
		{Query: "b", DependsOn: []int{2}},
		{Query: "a"},
	}
	issues, order := validateAuthoredPlan(steps, allAgentsKnown)

	if len(issues) != 0 {
		t.Fatalf("unexpected issues: %+v", issues)
	}
	want := []int{2, 1, 0}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

// The soft/hard pin distinction has to survive into validation, not just onto
// the wire: they fail differently at runtime, so they must warn differently here.
func TestValidateAuthoredPlan_HardPinOnUnknownAgentIsFatalSoftIsNot(t *testing.T) {
	hard := []domain.Step{{Query: "a", PreferredAgent: "ghost", AgentPin: domain.PinHard}}
	issues, _ := validateAuthoredPlan(hard, noAgentsKnown)
	i, ok := issueWithCode(issues, IssueUnknownAgent)
	if !ok || !i.Fatal {
		t.Fatalf("hard pin on an unknown agent: issue=%+v ok=%v — it must be fatal, the step cannot fall back", i, ok)
	}

	soft := []domain.Step{{Query: "a", PreferredAgent: "ghost", AgentPin: domain.PinSoft}}
	issues, order := validateAuthoredPlan(soft, noAgentsKnown)
	i, ok = issueWithCode(issues, IssueUnknownAgent)
	if !ok {
		t.Fatal("a soft pin on an unknown agent should still WARN")
	}
	if i.Fatal {
		t.Fatal("a soft pin must not be fatal — it cascades to ordinary selection")
	}
	if order == nil {
		t.Fatal("a plan with only non-fatal issues must still return an execution order")
	}
}

// An empty pin reads as SOFT, so a malformed pin degrades to the weaker,
// non-stranding behaviour rather than killing a step on a typo.
func TestValidateAuthoredPlan_EmptyPinStrengthDegradesToSoft(t *testing.T) {
	steps := []domain.Step{{Query: "a", PreferredAgent: "ghost"}}
	issues, _ := validateAuthoredPlan(steps, noAgentsKnown)

	i, ok := issueWithCode(issues, IssueUnknownAgent)
	if !ok {
		t.Fatal("expected an unknown-agent warning")
	}
	if i.Fatal {
		t.Fatal("an unspecified pin strength must not be treated as hard")
	}
}

func TestValidateAuthoredPlan_EmptyQueryIsFatalUnlessThought(t *testing.T) {
	issues, _ := validateAuthoredPlan([]domain.Step{{Query: "  "}}, allAgentsKnown)
	if i, ok := issueWithCode(issues, IssueEmptyQuery); !ok || !i.Fatal {
		t.Fatalf("empty query: issue=%+v ok=%v, want fatal", i, ok)
	}

	// A thought step legitimately carries no instruction.
	issues, _ = validateAuthoredPlan([]domain.Step{{IsThought: true}}, allAgentsKnown)
	if _, ok := issueWithCode(issues, IssueEmptyQuery); ok {
		t.Fatal("a thought step must not be flagged for having no query")
	}
}

// Fan-out over a step you do not depend on may run before that output exists.
// Not fatal — the executor would run it — but it is almost certainly a mistake,
// and it is invisible until the expansion produces nothing.
func TestValidateAuthoredPlan_FanOutWithoutDependencyWarns(t *testing.T) {
	src := 0
	steps := []domain.Step{
		{Query: "list the files"},
		{Query: "handle {item}", FanOutOver: &src}, // no DependsOn
	}
	issues, order := validateAuthoredPlan(steps, allAgentsKnown)

	i, ok := issueWithCode(issues, IssueBadFanOut)
	if !ok {
		t.Fatalf("no fan-out issue in %+v", issues)
	}
	if i.Fatal {
		t.Fatal("a missing fan-out dependency should warn, not block — the executor still runs it")
	}
	if order == nil {
		t.Fatal("a warning must not suppress the execution order")
	}

	// With the dependency declared, no complaint.
	steps[1].DependsOn = []int{0}
	issues, _ = validateAuthoredPlan(steps, allAgentsKnown)
	if _, ok := issueWithCode(issues, IssueBadFanOut); ok {
		t.Fatalf("declared dependency still warned: %+v", issues)
	}
}

func TestValidateAuthoredPlan_FanOutOverSelfIsFatal(t *testing.T) {
	self := 0
	steps := []domain.Step{{Query: "a", FanOutOver: &self}}
	issues, _ := validateAuthoredPlan(steps, allAgentsKnown)

	if i, ok := issueWithCode(issues, IssueBadFanOut); !ok || !i.Fatal {
		t.Fatalf("fan-out over self: issue=%+v ok=%v, want fatal", i, ok)
	}
}

// ── the handler ──────────────────────────────────────────────────────────────

type stubSubmitter struct {
	called   bool
	gotSteps []domain.Step
	knownFn  func(string) bool
}

func (s *stubSubmitter) SubmitPlan(_ context.Context, sessionID, subject string, steps []domain.Step) (string, string, error) {
	s.called = true
	s.gotSteps = steps
	return "sess-1", "plan-1", nil
}
func (s *stubSubmitter) KnownAgent(id string) bool {
	if s.knownFn == nil {
		return true
	}
	return s.knownFn(id)
}

func newSubmitService(sub PlanSubmitter) *Service {
	s := &Service{audit: NewInMemoryAuditStore(), feed: NewSpool(SpoolConfig{})}
	s.SetPlanSubmitter(sub)
	return s
}

// A dry run must not create, start or spend. The console's header promises this
// explicitly, so a side effect here would make the product lie.
func TestSubmitPlan_DryRunHasNoSideEffects(t *testing.T) {
	sub := &stubSubmitter{}
	s := newSubmitService(sub)

	resp, err := s.SubmitPlan(context.Background(), &pb.SubmitPlanOpRequest{
		CommandId: "c1", Reason: "checking the shape", DryRun: true,
		Steps: []*pb.AuthoredStepOp{{Query: "a", FanOutOver: -1}},
	})
	if err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}
	if sub.called {
		t.Fatal("a dry run reached the submitter — nothing may be created or started")
	}
	if !resp.GetAccepted() {
		t.Fatal("a valid plan should report accepted even on a dry run")
	}
	if resp.GetSessionId() != "" || resp.GetPlanId() != "" {
		t.Fatalf("dry run returned session=%q plan=%q, want both empty", resp.GetSessionId(), resp.GetPlanId())
	}
}

// A fatal plan must not run, dry_run or not — and it must say why.
func TestSubmitPlan_FatalPlanIsNotRun(t *testing.T) {
	sub := &stubSubmitter{}
	s := newSubmitService(sub)

	resp, err := s.SubmitPlan(context.Background(), &pb.SubmitPlanOpRequest{
		CommandId: "c1", Reason: "r",
		Steps: []*pb.AuthoredStepOp{
			{Query: "a", DependsOn: []int32{1}, FanOutOver: -1},
			{Query: "b", DependsOn: []int32{0}, FanOutOver: -1},
		},
	})
	if err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}
	if sub.called {
		t.Fatal("a plan with a cycle was submitted for execution")
	}
	if resp.GetAccepted() {
		t.Fatal("accepted = true for an unrunnable plan")
	}
	if len(resp.GetIssues()) == 0 {
		t.Fatal("rejected with no issues — the operator has nothing to fix")
	}
}

func TestSubmitPlan_ValidPlanRuns(t *testing.T) {
	sub := &stubSubmitter{}
	s := newSubmitService(sub)

	resp, err := s.SubmitPlan(context.Background(), &pb.SubmitPlanOpRequest{
		CommandId: "c1", Reason: "run it",
		Steps: []*pb.AuthoredStepOp{{Query: "do the thing", FanOutOver: -1}},
	})
	if err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}
	if !sub.called {
		t.Fatal("a valid plan never reached the submitter")
	}
	if resp.GetSessionId() != "sess-1" || resp.GetPlanId() != "plan-1" {
		t.Fatalf("session=%q plan=%q", resp.GetSessionId(), resp.GetPlanId())
	}
}

// fan_out_over uses -1 as "absent" because proto3 has no absent int32 and 0 is a
// legitimate step index. Getting this wrong turns every ordinary step into a
// fan-out over step 0.
func TestSubmitPlan_FanOutSentinelDistinguishesAbsentFromStepZero(t *testing.T) {
	sub := &stubSubmitter{}
	s := newSubmitService(sub)

	_, err := s.SubmitPlan(context.Background(), &pb.SubmitPlanOpRequest{
		CommandId: "c1", Reason: "r",
		Steps: []*pb.AuthoredStepOp{
			{Query: "list", FanOutOver: -1},
			{Query: "each {item}", DependsOn: []int32{0}, FanOutOver: 0},
		},
	})
	if err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}
	if sub.gotSteps[0].FanOutOver != nil {
		t.Fatal("step with fan_out_over=-1 became a fan-out")
	}
	if sub.gotSteps[1].FanOutOver == nil || *sub.gotSteps[1].FanOutOver != 0 {
		t.Fatalf("step with fan_out_over=0 lost its source: %v", sub.gotSteps[1].FanOutOver)
	}
}

func TestSubmitPlan_UnwiredReturnsUnimplemented(t *testing.T) {
	s := &Service{}
	if _, err := s.SubmitPlan(context.Background(), &pb.SubmitPlanOpRequest{
		CommandId: "c", Reason: "r", Steps: []*pb.AuthoredStepOp{{Query: "a"}},
	}); codeOf(err) != codes.Unimplemented {
		t.Fatalf("code = %v, want Unimplemented", codeOf(err))
	}
}

func TestSubmitPlan_EmptyStepsRejected(t *testing.T) {
	s := newSubmitService(&stubSubmitter{})
	if _, err := s.SubmitPlan(context.Background(), &pb.SubmitPlanOpRequest{
		CommandId: "c", Reason: "r",
	}); codeOf(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", codeOf(err))
	}
}
