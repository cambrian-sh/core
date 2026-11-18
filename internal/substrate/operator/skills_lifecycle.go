package operator

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/skill/lifecycle"
)

// SkillLifecycleAdmin is the ADR-0139 D13 lifecycle as the operator plane uses
// it. *lifecycle.Service satisfies it.
type SkillLifecycleAdmin interface {
	Skills(ctx context.Context) ([]lifecycle.Skill, error)
	Proposals(ctx context.Context, f domain.SkillProposalFilter) ([]lifecycle.Proposal, int, error)
	Decide(ctx context.Context, in lifecycle.DecideInput) (lifecycle.MutationResult, error)
	SetLock(ctx context.Context, commandID, name string, locked bool, actor, reason string) (lifecycle.MutationResult, error)
	Ledger(ctx context.Context, skillName string, limit, offset int) ([]lifecycle.LedgerEntry, int, error)
	Rollback(ctx context.Context, commandID, name, entryID, actor, reason string) (lifecycle.MutationResult, error)
}

func lifecycleErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, lifecycle.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, lifecycle.ErrFailedPrecondition):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, lifecycle.ErrInvalidArgument):
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return status.Errorf(codes.Internal, "skill lifecycle: %v", err)
}

func ts(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

// skillCommand is the shared shape of the four lifecycle mutations: validate,
// apply through the lifecycle (idempotent on command_id at the ledger itself),
// then write the audit row (idempotent on command_id at the audit store), which
// also emits the AuditEvent on the feed. A refused command (precondition,
// not-found) writes no audit row, so the same command_id can be retried once the
// operator has fixed what was wrong.
func (s *Service) skillCommand(ctx context.Context, commandID, reason, action, targetType, targetID string,
	apply func(actor string) (lifecycle.MutationResult, error)) (lifecycle.MutationResult, error) {
	if s.skillLifecycle == nil {
		return lifecycle.MutationResult{}, status.Error(codes.Unimplemented, "this kernel has no skill lifecycle store (capability skills-lifecycle)")
	}
	if s.audit == nil {
		return lifecycle.MutationResult{}, status.Error(codes.Unimplemented, "operator audit store not configured")
	}
	if commandID == "" || reason == "" {
		return lifecycle.MutationResult{}, status.Error(codes.InvalidArgument, "command_id and reason are required")
	}
	actor, role, _ := PrincipalFromContext(ctx)
	res, err := apply(actor)
	if err != nil {
		return lifecycle.MutationResult{}, lifecycleErr(err)
	}
	after, _ := json.Marshal(map[string]string{"skill_version": res.Version, "ledger_entry_id": res.LedgerEntryID})
	auditDeduped, err := s.recordAndEmit(ctx, domain.AuditEntry{
		ID: newAuditID(), CommandID: commandID, At: time.Now().UTC(),
		Actor: actor, Role: string(role), ActionType: action,
		TargetType: targetType, TargetID: targetID, After: string(after), Reason: reason, Result: "ok",
	})
	if err != nil {
		return lifecycle.MutationResult{}, err
	}
	res.Deduped = res.Deduped || auditDeduped
	return res, nil
}

// ListSkillProposals lists the staged approval queue (default: pending).
func (s *Service) ListSkillProposals(ctx context.Context, req *pb.ListSkillProposalsOpRequest) (*pb.ListSkillProposalsOpResponse, error) {
	if s.skillLifecycle == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel has no skill lifecycle store (capability skills-lifecycle)")
	}
	ps, _, err := s.skillLifecycle.Proposals(ctx, domain.SkillProposalFilter{State: req.GetState(), SkillName: req.GetSkillName()})
	if err != nil {
		return nil, lifecycleErr(err)
	}
	page, lo, hi := paginate(len(ps), req.GetPage(), req.GetPageSize())
	out := make([]*pb.SkillProposalOp, 0, hi-lo)
	for _, p := range ps[lo:hi] {
		out = append(out, &pb.SkillProposalOp{
			Id: p.ID, Name: p.Name, SkillMd: p.SkillMD, Rationale: p.Rationale, State: p.State,
			ProposerAgentId: p.ProposerAgentID, PrincipalId: p.PrincipalID, SourceRunId: p.SourceRunID,
			EvidenceCids: p.EvidenceCIDs, BaseVersion: p.BaseVersion, CurrentVersion: p.CurrentVersion,
			ScanVerdict: p.ScanVerdict, ScanFindings: p.ScanFindings,
			CreatedAt: ts(p.CreatedAt), DecidedAt: ts(p.DecidedAt), DecidedBy: p.DecidedBy, DecisionNote: p.DecisionNote,
		})
	}
	return &pb.ListSkillProposalsOpResponse{Proposals: out, Total: int32(len(ps)), Page: page}, nil
}

// DecideSkillProposal approves or rejects one PENDING proposal (Operator-only:
// not a List/Get read, so the interceptor's fail-closed rule gates it).
func (s *Service) DecideSkillProposal(ctx context.Context, req *pb.DecideSkillProposalOpRequest) (*pb.DecideSkillProposalOpResponse, error) {
	if req.GetProposalId() == "" && s.skillLifecycle != nil {
		return nil, status.Error(codes.InvalidArgument, "proposal_id is required")
	}
	action := "reject_skill_proposal"
	if req.GetApprove() {
		action = "approve_skill_proposal"
	}
	res, err := s.skillCommand(ctx, req.GetCommandId(), req.GetReason(), action, "skill_proposal", req.GetProposalId(),
		func(actor string) (lifecycle.MutationResult, error) {
			return s.skillLifecycle.Decide(ctx, lifecycle.DecideInput{
				CommandID: req.GetCommandId(), ProposalID: req.GetProposalId(), Approve: req.GetApprove(),
				LockAfter: req.GetLockAfterApprove(), Actor: actor, Reason: req.GetReason(),
			})
		})
	if err != nil {
		return nil, err
	}
	return &pb.DecideSkillProposalOpResponse{CommandId: req.GetCommandId(), Deduped: res.Deduped,
		SkillVersion: res.Version, LedgerEntryId: res.LedgerEntryID}, nil
}

// SetSkillLock locks or unlocks a skill against AGENT edits.
func (s *Service) SetSkillLock(ctx context.Context, req *pb.SetSkillLockOpRequest) (*pb.CommandAck, error) {
	if req.GetSkillName() == "" && s.skillLifecycle != nil {
		return nil, status.Error(codes.InvalidArgument, "skill_name is required")
	}
	action := "unlock_skill"
	if req.GetLocked() {
		action = "lock_skill"
	}
	res, err := s.skillCommand(ctx, req.GetCommandId(), req.GetReason(), action, "skill", req.GetSkillName(),
		func(actor string) (lifecycle.MutationResult, error) {
			return s.skillLifecycle.SetLock(ctx, req.GetCommandId(), req.GetSkillName(), req.GetLocked(), actor, req.GetReason())
		})
	if err != nil {
		return nil, err
	}
	return &pb.CommandAck{CommandId: req.GetCommandId(), Deduped: res.Deduped}, nil
}

// ListSkillLedger lists the append-only mutation ledger, newest first.
func (s *Service) ListSkillLedger(ctx context.Context, req *pb.ListSkillLedgerOpRequest) (*pb.ListSkillLedgerOpResponse, error) {
	if s.skillLifecycle == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel has no skill lifecycle store (capability skills-lifecycle)")
	}
	size := int(req.GetPageSize())
	if size <= 0 {
		size = defaultReadPageSize
	}
	page := pageOf(req.GetPage())
	es, total, err := s.skillLifecycle.Ledger(ctx, req.GetSkillName(), size, (int(page)-1)*size)
	if err != nil {
		return nil, lifecycleErr(err)
	}
	out := make([]*pb.SkillLedgerEntryOp, 0, len(es))
	for _, e := range es {
		out = append(out, &pb.SkillLedgerEntryOp{
			Id: e.ID, SkillName: e.SkillName, Mutation: e.Mutation, FromVersion: e.FromVersion, ToVersion: e.ToVersion,
			FromState: e.FromState, ToState: e.ToState, Actor: e.Actor, Reason: e.Reason, At: ts(e.At),
			ProposalId: e.ProposalID, RollbackOf: e.RollbackOf, Restorable: e.Restorable,
		})
	}
	return &pb.ListSkillLedgerOpResponse{Entries: out, Total: int32(total), Page: page}, nil
}

// RollbackSkill restores a skill to one ledger entry's state, as a new entry.
func (s *Service) RollbackSkill(ctx context.Context, req *pb.RollbackSkillOpRequest) (*pb.RollbackSkillOpResponse, error) {
	if (req.GetSkillName() == "" || req.GetLedgerEntryId() == "") && s.skillLifecycle != nil {
		return nil, status.Error(codes.InvalidArgument, "skill_name and ledger_entry_id are required")
	}
	res, err := s.skillCommand(ctx, req.GetCommandId(), req.GetReason(), "rollback_skill", "skill", req.GetSkillName(),
		func(actor string) (lifecycle.MutationResult, error) {
			return s.skillLifecycle.Rollback(ctx, req.GetCommandId(), req.GetSkillName(), req.GetLedgerEntryId(), actor, req.GetReason())
		})
	if err != nil {
		return nil, err
	}
	return &pb.RollbackSkillOpResponse{CommandId: req.GetCommandId(), Deduped: res.Deduped,
		LedgerEntryId: res.LedgerEntryID, SkillVersion: res.Version}, nil
}

// skillOpLifecycle fills the D13 fields of a SkillOp from its record.
func skillOpLifecycle(op *pb.SkillOp, r lifecycle.Skill) {
	op.Version, op.Source, op.LifecycleState = r.Version, r.Source, r.State
	op.StateChangedAt, op.Locked, op.ExemptReason = ts(r.StateChangedAt), r.Locked, r.ExemptReason
	op.UseCount, op.SuccessCount, op.LastUsedAt = r.UseCount, r.SuccessCount, ts(r.LastUsedAt)
	op.LedgerEntryId = r.LedgerEntryID
}

// skillListFilter applies ListSkillsOpRequest's D13 filters to one SkillOp.
// Empty lifecycle_state = active and stale (what ListSkills returned before);
// include_archived widens that to archived too.
func skillListFilter(req *pb.ListSkillsOpRequest, op *pb.SkillOp) bool {
	if src := req.GetSource(); src != "" && op.GetSource() != src {
		return false
	}
	switch st := req.GetLifecycleState(); {
	case st != "":
		return op.GetLifecycleState() == st
	case op.GetLifecycleState() == domain.SkillStateArchived:
		return req.GetIncludeArchived()
	}
	return true
}
