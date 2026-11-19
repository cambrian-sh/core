package operator

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// Validation issue codes. Stable machine tokens — a console keys its copy off
// these, so they are contract, not log text.
const (
	IssueCycle             = "cycle"
	IssueOutOfBoundsDep    = "out_of_bounds_dependency"
	IssueEmptyQuery        = "empty_query"
	IssueUnknownAgent      = "unknown_agent"
	IssueUnknownCapability = "unknown_capability"
	IssueBadFanOut         = "bad_fan_out"
	// IssueRunsAsOneBrief (advisory, whole plan) says the kernel's default lane
	// will run the plan as ONE brief for one agent (ENG-2246, ENG-1377), not
	// step by step.
	IssueRunsAsOneBrief = "runs_as_one_brief"
)

// OneBriefRunner is optionally implemented by a PlanSubmitter whose kernel runs
// an authored plan as one brief (the ADR-0150 single-agent lane, the default).
// SubmitPlan then adds the advisory IssueRunsAsOneBrief, so the console states
// what will happen instead of promising step semantics the lane does not have.
type OneBriefRunner interface {
	RunsAsOneBrief() bool
}

// oneBriefMessage is the advisory's text: the honest description of how the
// single-agent lane runs an authored plan (ENG-1377, ENG-2246).
const oneBriefMessage = "This kernel runs on the single-agent lane (execution.lane.default: single_agent), " +
	"which runs an authored plan as ONE brief: one agent receives every step, in the order written, and may " +
	"fan work out itself. Hard pins (agent_pin), parallelism from depends_on and fan_out_over are guidance in " +
	"the brief, not enforced per step; a hard-pinned agent is not required to run. The session closes with the " +
	"outcome when the agent finishes or fails. To dispatch the steps one by one, run the planner lane " +
	"(execution.lane.default: planner)."

// PlanSubmitter runs an operator-authored plan.
//
// Validation lives on the SERVICE side (see validateAuthoredPlan) rather than
// here, because it must be identical for a dry run and a real submit. Splitting
// it would let a plan pass its preview and fail its submission, which is the one
// outcome a dry run exists to prevent.
type PlanSubmitter interface {
	// SubmitPlan attaches the plan to sessionID (creating one when empty) and
	// starts it. Returns the session and plan ids.
	SubmitPlan(ctx context.Context, sessionID, subject string, steps []domain.Step) (sessID, planID string, err error)
	// KnownAgent reports whether an agent id exists, for pin validation. A pin on
	// a non-existent agent is worth flagging BEFORE it strands a step at runtime.
	KnownAgent(ctx context.Context, id string) bool
}

// SubmitPlan validates and (unless dry_run) runs an operator-authored plan.
func (s *Service) SubmitPlan(ctx context.Context, req *pb.SubmitPlanOpRequest) (*pb.SubmitPlanOpResponse, error) {
	if s.planSubmitter == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel does not accept operator-authored plans")
	}
	if len(req.GetSteps()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "steps is required")
	}

	steps := authoredStepsToDomain(req.GetSteps())
	issues, order := validateAuthoredPlan(steps, func(id string) bool {
		return s.planSubmitter.KnownAgent(ctx, id)
	})
	if b, ok := s.planSubmitter.(OneBriefRunner); ok && b.RunsAsOneBrief() {
		issues = append(issues, PlanIssue{StepIndex: -1, Code: IssueRunsAsOneBrief, Message: oneBriefMessage})
	}

	fatal := false
	for _, i := range issues {
		if i.Fatal {
			fatal = true
			break
		}
	}

	// A dry run and a fatal plan take the same early exit: nothing is created,
	// started or spent. They are reported differently — accepted distinguishes
	// "valid, and I did not run it" from "I could not run it" — but neither has
	// side effects, so neither is audited as a mutation.
	if req.GetDryRun() || fatal {
		return &pb.SubmitPlanOpResponse{
			CommandId:      req.GetCommandId(),
			Accepted:       !fatal,
			Issues:         issuesToOp(issues),
			ExecutionOrder: int32Slice(order),
		}, nil
	}

	var sessID, planID string
	ack, err := s.runMutation(ctx, req.GetCommandId(), req.GetReason(),
		"submit_plan", "session", req.GetSessionId(), planSummary(steps),
		func() error {
			var subErr error
			sessID, planID, subErr = s.planSubmitter.SubmitPlan(ctx, req.GetSessionId(), req.GetSubject(), steps)
			return subErr
		})
	if err != nil {
		return nil, err
	}
	return &pb.SubmitPlanOpResponse{
		CommandId:      ack.GetCommandId(),
		Deduped:        ack.GetDeduped(),
		Accepted:       true,
		SessionId:      sessID,
		PlanId:         planID,
		Issues:         issuesToOp(issues),
		ExecutionOrder: int32Slice(order),
	}, nil
}

// authoredStepsToDomain maps the wire form onto domain.Step.
//
// The mapping is one-to-one by design, so this function stays a translation and
// never becomes a place where the two schemas quietly diverge.
func authoredStepsToDomain(in []*pb.AuthoredStepOp) []domain.Step {
	out := make([]domain.Step, 0, len(in))
	for _, a := range in {
		deps := make([]int, 0, len(a.GetDependsOn()))
		for _, d := range a.GetDependsOn() {
			deps = append(deps, int(d))
		}
		st := domain.Step{
			Query:                a.GetQuery(),
			RequiredCapabilities: a.GetRequiredCapabilities(),
			DependsOn:            deps,
			MaxEnergy:            a.GetMaxEnergy(),
			CheckpointAfter:      a.GetCheckpointAfter(),
			// AuthoredStepOp field 7 (checkpoint_query) is reserved since contract
			// 0102: domain.Step.CheckpointQuery was deleted 2026-11-14 (ENG-1313;
			// nothing read it since ADR-0139 D10).
			IsThought:      a.GetIsThought(),
			PreferredAgent: a.GetPreferredAgent(),
			AgentPin:       a.GetAgentPin(),
			FanOutVar:      a.GetFanOutVar(),
		}
		// -1 is the wire's "not a fan-out": proto3 has no absent int32, and 0 is
		// a legitimate step index, so a sentinel is the only way to tell "fan out
		// over step 0" from "do not fan out".
		if fo := a.GetFanOutOver(); fo >= 0 {
			idx := int(fo)
			st.FanOutOver = &idx
		}
		out = append(out, st)
	}
	return out
}

func issuesToOp(in []PlanIssue) []*pb.PlanValidationIssueOp {
	out := make([]*pb.PlanValidationIssueOp, 0, len(in))
	for _, i := range in {
		out = append(out, &pb.PlanValidationIssueOp{
			StepIndex: int32(i.StepIndex),
			Code:      i.Code,
			Message:   i.Message,
			Fatal:     i.Fatal,
		})
	}
	return out
}

func int32Slice(in []int) []int32 {
	out := make([]int32, 0, len(in))
	for _, v := range in {
		out = append(out, int32(v))
	}
	return out
}
