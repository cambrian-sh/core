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
)

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
	KnownAgent(id string) bool
}

// SetPlanSubmitter wires operator-authored plan submission. nil ⇒ SubmitPlan
// returns Unimplemented, which the console renders as "this kernel accepts only
// planner-authored plans" rather than a Run button that fails.
func (s *Service) SetPlanSubmitter(p PlanSubmitter) { s.planSubmitter = p }

// HasPlanSubmitter reports whether authored plans can be run.
func (s *Service) HasPlanSubmitter() bool { return s.planSubmitter != nil }

// SubmitPlan validates and (unless dry_run) runs an operator-authored plan.
func (s *Service) SubmitPlan(ctx context.Context, req *pb.SubmitPlanOpRequest) (*pb.SubmitPlanOpResponse, error) {
	if s.planSubmitter == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel does not accept operator-authored plans")
	}
	if len(req.GetSteps()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "steps is required")
	}

	steps := authoredStepsToDomain(req.GetSteps())
	issues, order := validateAuthoredPlan(steps, s.planSubmitter.KnownAgent)

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
			CheckpointQuery:      a.GetCheckpointQuery(),
			IsThought:            a.GetIsThought(),
			PreferredAgent:       a.GetPreferredAgent(),
			AgentPin:             a.GetAgentPin(),
			FanOutVar:            a.GetFanOutVar(),
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
