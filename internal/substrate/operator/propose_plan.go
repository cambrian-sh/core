package operator

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// PlanProposer turns a goal into a plan WITHOUT committing to it.
//
// Every method here must be side-effect free. The console's header promises
// "nothing committed yet — no session, no plan, no spend", and the whole point of
// separating this from CreateSession is that the promise is enforceable rather
// than merely stated.
type PlanProposer interface {
	// Clarify returns the questions a goal needs answered, or nil when it is
	// answerable. Called BEFORE planning, because planner output is the ceiling
	// on routing quality and vagueness cannot be recovered downstream.
	Clarify(ctx context.Context, goal string, answers []string) ([]ProposedQuestion, error)

	// Propose builds a candidate plan. Never creates a session or starts one.
	Propose(ctx context.Context, goal string, answers []string) (ProposedPlan, error)

	// AccessConsequence describes what this goal would read and write, in the
	// caller's own terms. Returned even when the plan is fine — the point is that
	// the operator learns it BEFORE the answer quotes a customer record.
	AccessConsequence(ctx context.Context, goal string) string
}

// ProposedQuestion is one clarification.
type ProposedQuestion struct {
	Question              string
	Kind                  string
	WhyItChangesTheAnswer string
	Options               []ProposedOption
}

// ProposedOption is one answer, with a real count where one exists.
type ProposedOption struct {
	Label string
	// DocumentCount is -1 when uncountable. Distinct from 0: a zero-document
	// option is a real and alarming answer, "we could not count" is not.
	DocumentCount int
	Detail        string
}

// ProposedPlan is a candidate plan plus what running it would cost.
type ProposedPlan struct {
	Steps           []domain.Step
	EstimatedTokens int64
	EstimatedWallMs int64
	MaxParallel     int
	AccessNote      string
}

// SetPlanProposer wires the propose-only path. nil ⇒ ProposePlan returns
// Unimplemented, which a console renders as "this kernel plans on submit only"
// rather than a proposal pane that never fills.
func (s *Service) SetPlanProposer(p PlanProposer) { s.planProposer = p }

// HasPlanProposer reports whether propose-without-committing is available.
func (s *Service) HasPlanProposer() bool { return s.planProposer != nil }

// ProposePlan returns questions or a proposal. Creates nothing.
//
// No command_id: nothing is mutated, so there is nothing to dedupe and nothing
// worth an audit row. Adding one would imply a side effect that does not exist.
func (s *Service) ProposePlan(ctx context.Context, req *pb.ProposePlanOpRequest) (*pb.ProposePlanOpResponse, error) {
	if s.planProposer == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel does not support proposing a plan without starting it")
	}
	if req.GetGoal() == "" {
		return nil, status.Error(codes.InvalidArgument, "goal is required")
	}

	// The access consequence is computed FIRST and returned on every branch,
	// including the question branch. An operator deciding how to narrow a scope
	// should already know what the broad answer would expose.
	out := &pb.ProposePlanOpResponse{
		AccessConsequence: s.planProposer.AccessConsequence(ctx, req.GetGoal()),
	}

	questions, err := s.planProposer.Clarify(ctx, req.GetGoal(), req.GetAnswers())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "clarify: %v", err)
	}
	if len(questions) > 0 {
		// Questions INSTEAD of a proposal, never alongside one. Offering a runnable
		// plan next to "I need to ask you something" invites approving the plan and
		// skipping the question, which is exactly the outcome the interview exists
		// to prevent.
		for _, q := range questions {
			out.Questions = append(out.Questions, questionToOp(q))
		}
		return out, nil
	}

	proposal, err := s.planProposer.Propose(ctx, req.GetGoal(), req.GetAnswers())
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "propose: %v", err)
	}
	out.Proposal = &pb.PlanProposalOp{
		Steps:           domainStepsToAuthored(proposal.Steps),
		EstimatedTokens: proposal.EstimatedTokens,
		EstimatedWallMs: proposal.EstimatedWallMs,
		MaxParallel:     int32(proposal.MaxParallel),
		AccessNote:      proposal.AccessNote,
	}
	return out, nil
}

func questionToOp(q ProposedQuestion) *pb.InterviewQuestionOp {
	op := &pb.InterviewQuestionOp{
		Question:              q.Question,
		Kind:                  q.Kind,
		WhyItChangesTheAnswer: q.WhyItChangesTheAnswer,
	}
	for _, o := range q.Options {
		op.Options = append(op.Options, &pb.InterviewOptionOp{
			Label:         o.Label,
			Detail:        o.Detail,
			DocumentCount: int32(o.DocumentCount),
		})
	}
	return op
}

// domainStepsToAuthored is the inverse of authoredStepsToDomain, so a proposal
// comes back in exactly the shape SubmitPlan accepts.
//
// That symmetry is the point: "Run it" on a proposal card is a straight handoff
// to SubmitPlan with no reshaping, and a field that survived one direction but
// not the other would be silently dropped on approval.
func domainStepsToAuthored(steps []domain.Step) []*pb.AuthoredStepOp {
	out := make([]*pb.AuthoredStepOp, 0, len(steps))
	for _, st := range steps {
		deps := make([]int32, 0, len(st.DependsOn))
		for _, d := range st.DependsOn {
			deps = append(deps, int32(d))
		}
		a := &pb.AuthoredStepOp{
			Query:                st.Query,
			RequiredCapabilities: st.RequiredCapabilities,
			DependsOn:            deps,
			MaxEnergy:            st.MaxEnergy,
			RecommendedModel:     st.RecommendedModel,
			CheckpointAfter:      st.CheckpointAfter,
			CheckpointQuery:      st.CheckpointQuery,
			IsThought:            st.IsThought,
			PreferredAgent:       st.PreferredAgent,
			AgentPin:             st.AgentPin,
			FanOutVar:            st.FanOutVar,
			// -1 is the absent sentinel, matching SubmitPlan. Emitting 0 here would
			// turn every ordinary step into a fan-out over step 0 on the round trip.
			FanOutOver: -1,
		}
		if st.FanOutOver != nil {
			a.FanOutOver = int32(*st.FanOutOver)
		}
		out = append(out, a)
	}
	return out
}
