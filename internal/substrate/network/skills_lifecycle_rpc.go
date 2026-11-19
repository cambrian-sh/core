package network

import (
	"context"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/skill/lifecycle"
)

// SkillLifecycle is the slice of the ADR-0139 D13 lifecycle the agent plane
// uses. *lifecycle.Service satisfies it.
type SkillLifecycle interface {
	Propose(ctx context.Context, in lifecycle.ProposeInput) (lifecycle.ProposeResult, error)
	RecordUse(ctx context.Context, name, runID string) error
	RecordRunOutcome(ctx context.Context, runID string, success bool) error
}

var rejectionToProto = map[lifecycle.Rejection]pb.SkillProposalRejection{
	lifecycle.RejectNone:          pb.SkillProposalRejection_SKILL_PROPOSAL_REJECTION_UNSPECIFIED,
	lifecycle.RejectInvalidFormat: pb.SkillProposalRejection_SKILL_PROPOSAL_REJECTION_INVALID_FORMAT,
	lifecycle.RejectNameTaken:     pb.SkillProposalRejection_SKILL_PROPOSAL_REJECTION_NAME_TAKEN,
	lifecycle.RejectLocked:        pb.SkillProposalRejection_SKILL_PROPOSAL_REJECTION_LOCKED,
	lifecycle.RejectScanDangerous: pb.SkillProposalRejection_SKILL_PROPOSAL_REJECTION_SCAN_DANGEROUS,
	lifecycle.RejectQuota:         pb.SkillProposalRejection_SKILL_PROPOSAL_REJECTION_QUOTA,
	lifecycle.RejectDenied:        pb.SkillProposalRejection_SKILL_PROPOSAL_REJECTION_DENIED,
}

// ProposeSkill queues an agent-proposed skill as PENDING (ADR-0139 D13). It is
// never active until an operator approves it (OperatorConsole/DecideSkillProposal).
//
// Provenance is the kernel's, not the agent's: the proposing agent, its session
// and the SOURCE RUN come from the binding of the presented lease, resolved under
// the ADR-0139 D8 ownership rule (an agent may present only a lease bound to
// itself). A refusal is an OK response with accepted=false and a rejection — the
// shape a tool result can show the model; only a malformed request is an error.
func (s *Server) ProposeSkill(ctx context.Context, req *pb.ProposeSkillRequest) (*pb.ProposeSkillResponse, error) {
	if s.SkillLifecycle == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel has no skill lifecycle store (capability skills-lifecycle)")
	}
	lease := domain.LeaseID(req.GetLeaseId())
	if lease == "" {
		lease = domain.LeaseID(mdValue(ctx, domain.LeaseHeader))
	}
	if lease == "" {
		// No lease is refused like an invalid one on every agent-plane RPC (ENG-2171).
		return nil, status.Error(codes.PermissionDenied, "a lease is required")
	}
	if req.GetName() == "" || req.GetSkillMd() == "" {
		return nil, status.Error(codes.InvalidArgument, "name and skill_md are required")
	}
	deny := func(detail string) *pb.ProposeSkillResponse {
		return &pb.ProposeSkillResponse{Rejection: pb.SkillProposalRejection_SKILL_PROPOSAL_REJECTION_DENIED, Detail: detail}
	}
	binding, ok := s.resolveLeaseFor(ctx, lease)
	if !ok {
		// The same answer for an unknown, expired and foreign lease (D8: the caller
		// learns nothing about whether someone else's lease was live).
		return deny("the lease is not live for this caller"), nil
	}
	agentID := binding.AgentID
	if agentID == "" {
		// A non-agent principal (an in-process kernel caller) on an agent-unbound
		// lease; agent callers never reach here (resolveLeaseFor fails closed).
		agentID, _ = callerAgentIdentity(ctx)
	}
	if agentID == "" {
		return deny("the lease names no agent"), nil
	}
	// The run principal comes from the ONE lease → principal resolution the
	// kernel has (the D14 binding: conversation owner → task beneficiary →
	// configured default), not a second derivation. "" when none is wired.
	principal := ""
	if s.MemoryNotesPrincipal != nil {
		principal = s.MemoryNotesPrincipal(ctx, binding)
	}
	res, err := s.SkillLifecycle.Propose(ctx, lifecycle.ProposeInput{
		Name: req.GetName(), SkillMD: req.GetSkillMd(), Rationale: req.GetRationale(),
		EvidenceCIDs: req.GetEvidenceCids(), BaseVersion: req.GetBaseVersion(),
		AgentID: agentID, PrincipalID: principal, RunID: string(binding.RunID), SessionID: string(binding.SessionID),
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "propose skill: %v", err)
	}
	return &pb.ProposeSkillResponse{
		Accepted: res.Accepted, ProposalId: res.ProposalID,
		Rejection: rejectionToProto[res.Rejection], Detail: res.Detail,
	}, nil
}

// recordSkillUse counts a use_skill (Tier-2) load (ADR-0139 D13 telemetry). The
// run is taken from the lease the SDK sends with the load (x-session-token, the
// ADR-0046 D6 grant key, which is the step's lease), so the run's outcome can
// later credit it. Best effort: telemetry never fails a skill load.
func (s *Server) recordSkillUse(ctx context.Context, name string) {
	if s.SkillLifecycle == nil {
		return
	}
	if err := s.SkillLifecycle.RecordUse(context.WithoutCancel(ctx), name, s.skillRunID(ctx)); err != nil {
		slog.WarnContext(ctx, "ADR-0139 D13: skill use not recorded", "skill", name, "err", err)
	}
}

// skillRunID is the run of the lease a skill load presents (x-session-token, the
// ADR-0046 D6 grant key, else the lease header), resolved under the ADR-0139 D8
// ownership rule; "" when none resolves.
func (s *Server) skillRunID(ctx context.Context) string {
	lease := domain.LeaseID(mdValue(ctx, "x-session-token"))
	if lease == "" {
		lease = domain.LeaseID(mdValue(ctx, domain.LeaseHeader))
	}
	if lease != "" {
		if b, ok := s.resolveLeaseFor(ctx, lease); ok {
			return string(b.RunID)
		}
	}
	return ""
}

// recordSkillRunOutcome settles the run's skill uses (success_count). Plan runs
// only: a chat turn has no run outcome, so its loads count as uses, never as
// successes.
func (s *Server) recordSkillRunOutcome(ctx context.Context, runID domain.RunID, success bool) {
	if s.SkillLifecycle == nil || runID == "" {
		return
	}
	if err := s.SkillLifecycle.RecordRunOutcome(context.WithoutCancel(ctx), string(runID), success); err != nil {
		slog.WarnContext(ctx, "ADR-0139 D13: skill run outcome not recorded", "run", runID, "err", err)
	}
}
