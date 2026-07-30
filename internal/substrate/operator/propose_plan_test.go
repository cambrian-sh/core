package operator

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

type stubProposer struct {
	questions   []ProposedQuestion
	plan        ProposedPlan
	consequence string
	proposeErr  error
	// proposed records whether Propose was reached, so a test can prove the
	// question branch short-circuited rather than merely preferring questions.
	proposed bool
}

func (s *stubProposer) Clarify(context.Context, string, []string) ([]ProposedQuestion, error) {
	return s.questions, nil
}
func (s *stubProposer) Propose(context.Context, string, []string) (ProposedPlan, error) {
	s.proposed = true
	return s.plan, s.proposeErr
}
func (s *stubProposer) AccessConsequence(context.Context, string) string { return s.consequence }

func newProposeService(p PlanProposer) *Service {
	s := &Service{}
	s.SetPlanProposer(p)
	return s
}

// Questions must come back INSTEAD of a proposal, never alongside one. Offering a
// runnable plan next to "I need to ask you something" invites approving the plan
// and skipping the question, which is the outcome the interview exists to stop.
func TestProposePlan_QuestionsSuppressTheProposal(t *testing.T) {
	sp := &stubProposer{questions: []ProposedQuestion{{
		Question:              "Which quarter?",
		Kind:                  "scope",
		WhyItChangesTheAnswer: "Q2 alone is 61 documents; everything is 1204.",
		Options: []ProposedOption{
			{Label: "Q2 only", DocumentCount: 61, Detail: "61 documents"},
			{Label: "Everything", DocumentCount: 1204, Detail: "1204 documents"},
		},
	}}}
	s := newProposeService(sp)

	resp, err := s.ProposePlan(context.Background(), &pb.ProposePlanOpRequest{Goal: "summarise the reports"})
	if err != nil {
		t.Fatalf("ProposePlan: %v", err)
	}
	if resp.GetProposal() != nil {
		t.Fatal("a proposal was returned alongside questions")
	}
	if sp.proposed {
		t.Fatal("Propose was called despite unanswered questions — planning a vague goal is what this avoids")
	}
	if len(resp.GetQuestions()) != 1 {
		t.Fatalf("got %d questions, want 1", len(resp.GetQuestions()))
	}
	q := resp.GetQuestions()[0]
	if q.GetWhyItChangesTheAnswer() == "" {
		t.Fatal("why_it_changes_the_answer is empty — the question reads as bureaucracy")
	}
	// The counts are what make this a decision rather than a preference.
	if q.GetOptions()[0].GetDocumentCount() != 61 {
		t.Fatalf("option count = %d, want 61", q.GetOptions()[0].GetDocumentCount())
	}
}

// The access consequence is volunteered on EVERY branch, including the question
// branch: an operator narrowing a scope should already know what the broad answer
// would expose.
func TestProposePlan_AccessConsequenceAccompaniesQuestionsToo(t *testing.T) {
	sp := &stubProposer{
		questions:   []ProposedQuestion{{Question: "Which quarter?"}},
		consequence: "refund logs carry customer-pii",
	}
	s := newProposeService(sp)

	resp, err := s.ProposePlan(context.Background(), &pb.ProposePlanOpRequest{Goal: "look at refunds"})
	if err != nil {
		t.Fatalf("ProposePlan: %v", err)
	}
	if resp.GetAccessConsequence() == "" {
		t.Fatal("access_consequence is empty on the question branch — the operator narrows scope blind")
	}
}

func TestProposePlan_AnswerableGoalReturnsAProposal(t *testing.T) {
	sp := &stubProposer{plan: ProposedPlan{
		Steps:           []domain.Step{{Query: "read the logs"}, {Query: "summarise", DependsOn: []int{0}}},
		EstimatedTokens: 4200,
		EstimatedWallMs: 8000,
		MaxParallel:     1,
	}}
	s := newProposeService(sp)

	resp, err := s.ProposePlan(context.Background(), &pb.ProposePlanOpRequest{Goal: "summarise Q2 refunds"})
	if err != nil {
		t.Fatalf("ProposePlan: %v", err)
	}
	if len(resp.GetQuestions()) != 0 {
		t.Fatal("questions returned for an answerable goal")
	}
	p := resp.GetProposal()
	if p == nil || len(p.GetSteps()) != 2 {
		t.Fatalf("proposal = %+v", p)
	}
	if p.GetEstimatedTokens() != 4200 {
		t.Fatalf("estimated_tokens = %d, want 4200", p.GetEstimatedTokens())
	}
}

// A proposal must come back in exactly the shape SubmitPlan accepts, so "Run it"
// is a straight handoff. A field surviving one direction but not the other would
// be silently dropped on approval.
func TestProposePlan_StepsRoundTripThroughSubmitPlanShape(t *testing.T) {
	src := 0
	sp := &stubProposer{plan: ProposedPlan{Steps: []domain.Step{
		{Query: "list"},
		{
			Query:                "handle {item}",
			DependsOn:            []int{0},
			RequiredCapabilities: []string{"analysis"},
			PreferredAgent:       "analyst",
			AgentPin:             domain.PinHard,
			FanOutOver:           &src,
			FanOutVar:            "item",
			CheckpointAfter:      true,
		},
	}}}
	s := newProposeService(sp)

	resp, err := s.ProposePlan(context.Background(), &pb.ProposePlanOpRequest{Goal: "g"})
	if err != nil {
		t.Fatalf("ProposePlan: %v", err)
	}
	steps := resp.GetProposal().GetSteps()

	// The absent sentinel must survive: emitting 0 would turn the first ordinary
	// step into a fan-out over step 0 on the round trip.
	if steps[0].GetFanOutOver() != -1 {
		t.Fatalf("step 0 fan_out_over = %d, want -1", steps[0].GetFanOutOver())
	}
	if steps[1].GetFanOutOver() != 0 || steps[1].GetFanOutVar() != "item" {
		t.Fatalf("step 1 fan-out = %d/%q", steps[1].GetFanOutOver(), steps[1].GetFanOutVar())
	}
	// The hard pin must survive — it is the difference between a step that dies
	// and one that cascades.
	if steps[1].GetAgentPin() != domain.PinHard || steps[1].GetPreferredAgent() != "analyst" {
		t.Fatalf("pin = %q/%q", steps[1].GetPreferredAgent(), steps[1].GetAgentPin())
	}

	// And it survives the trip BACK through SubmitPlan's own mapper.
	back := authoredStepsToDomain(steps)
	if back[0].FanOutOver != nil {
		t.Fatal("step 0 became a fan-out on the return trip")
	}
	if back[1].FanOutOver == nil || *back[1].FanOutOver != 0 {
		t.Fatalf("step 1 lost its fan-out source: %v", back[1].FanOutOver)
	}
	if back[1].AgentPin != domain.PinHard || !back[1].CheckpointAfter {
		t.Fatalf("round trip lost pin/checkpoint: %+v", back[1])
	}
}

func TestProposePlan_UnwiredReturnsUnimplemented(t *testing.T) {
	if _, err := (&Service{}).ProposePlan(context.Background(), &pb.ProposePlanOpRequest{Goal: "g"}); codeOf(err) != codes.Unimplemented {
		t.Fatalf("code = %v, want Unimplemented", codeOf(err))
	}
}

func TestProposePlan_RequiresGoal(t *testing.T) {
	s := newProposeService(&stubProposer{})
	if _, err := s.ProposePlan(context.Background(), &pb.ProposePlanOpRequest{}); codeOf(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", codeOf(err))
	}
}

// A planner failure is FailedPrecondition, not Internal: nothing is broken in the
// kernel and the operator's move is to rephrase, which is a different instruction
// from "retry" or "report a bug".
func TestProposePlan_PlannerFailureIsFailedPrecondition(t *testing.T) {
	s := newProposeService(&stubProposer{proposeErr: errors.New("model returned unparseable JSON")})

	if _, err := s.ProposePlan(context.Background(), &pb.ProposePlanOpRequest{Goal: "g"}); codeOf(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", codeOf(err))
	}
}
