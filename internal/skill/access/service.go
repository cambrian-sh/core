// Package access is per-agent skill access (owner decision 2026-11-05,
// cambrian-knowledge/decisions/skill-access-grants.md; amends ADR-0139 D13):
//
//	"when an agent tries to reach a skill from the kernel, the operator should
//	 approve, and after that, that agent should always have access to that skill."
//
// A profile agent's EFFECTIVE skill set is its profile's `skills` list plus its
// active grants (domain.SkillGrantStore). The agent plane (network.Server
// ListSkills) serves a skill body only inside that set; a use_skill on a skill
// outside it is a REQUEST, put to the operator on the same approval hub that
// dangerous-tool approvals use (domain.ApprovalController, Kind skill_access), and
// an approval becomes a durable grant keyed by agent id.
//
// Bounding mirrors domain.ProfileToolCeiling: an agent with a declarative profile
// is BOUND; an agent without one (a legacy or third-party agent that declares no
// skill list) is unbound and keeps the scope-gated access it had before
// (decision logged on T-20261105-approved-agent-proposed-skills-are-unreachable).
//
// It extends the skills plane rather than standing beside it (AGENTS.md §4.3):
// the catalog, scope gate and Tier-2 fetch are unchanged; this package only
// answers "is this skill in this agent's set, and if not, may it be?".
package access

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// Sentinel errors the operator transport maps onto gRPC codes.
var (
	ErrInvalidArgument    = errors.New("skill access: invalid argument")
	ErrNotFound           = errors.New("skill access: not found")
	ErrFailedPrecondition = errors.New("skill access: failed precondition")
)

// ProfileSkills reports an agent's profile skills and whether the agent is bound
// (has a declarative profile). Unbound agents are not limited by this package.
type ProfileSkills func(agentID string) (skills []string, bound bool)

// KnownSkill reports whether the kernel serves a skill by that name.
type KnownSkill func(name string) bool

// Config is execution.skills' access tuning.
type Config struct {
	// RequestTimeout is how long a use_skill request waits for an operator
	// decision before it answers "pending" (default 120 s).
	RequestTimeout time.Duration
	// AutoApprove grants every request without asking (execution.skills.
	// access_auto_approve). Separate from tools_auto_approve on purpose: that flag
	// is a decision about dangerous tools, not about widening what agents read.
	AutoApprove bool
}

// ApproverAuto is the granted_by of an auto-approved request.
const ApproverAuto = "auto-approve"

// Service answers effective-set questions and runs the request, grant and
// revoke flows. Construct with New; every dependency is a constructor argument
// (AGENTS.md §2.3).
type Service struct {
	store     domain.SkillGrantStore
	profiles  ProfileSkills
	known     KnownSkill
	approvals domain.ApprovalController
	cfg       Config
	now       func() time.Time
}

// New builds the service. profiles is required. store nil = no durable grants:
// bound agents get exactly their profile skills and every request is denied with
// a reason (fail closed). approvals nil = no live approval: requests stay
// pending. known nil = operator grants are not checked against the catalog.
func New(store domain.SkillGrantStore, profiles ProfileSkills, known KnownSkill,
	approvals domain.ApprovalController, cfg Config, now func() time.Time) (*Service, error) {
	if profiles == nil {
		return nil, fmt.Errorf("skill access: a profile skill resolver is required")
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 120 * time.Second
	}
	if now == nil {
		now = time.Now
	}
	return &Service{store: store, profiles: profiles, known: known, approvals: approvals, cfg: cfg, now: now}, nil
}

// HasStore reports whether grants are durable (the operator capability gate).
func (s *Service) HasStore() bool { return s != nil && s.store != nil }

// Effective returns the agent's effective skill set, name → access
// (domain.SkillAccessProfile or domain.SkillAccessGranted), and whether the agent
// is bound at all. For an unbound agent the map is nil and bound is false: the
// caller applies no set. A grant-store failure FAILS CLOSED to the profile
// skills alone.
func (s *Service) Effective(ctx context.Context, agentID string) (map[string]string, bool) {
	if s == nil {
		return nil, false
	}
	own, bound := s.profiles(agentID)
	if !bound {
		return nil, false
	}
	set := make(map[string]string, len(own))
	if s.store != nil {
		grants, err := s.store.ActiveGrants(ctx, agentID)
		if err != nil {
			slog.WarnContext(ctx, "skill access: grants unreadable; serving profile skills only (fail closed)",
				"agent_id", agentID, "err", err)
		}
		for _, g := range grants {
			set[g.SkillName] = domain.SkillAccessGranted
		}
	}
	for _, n := range own {
		set[n] = domain.SkillAccessProfile // a profile skill reads as "profile" even when also granted
	}
	return set, true
}

// RequestInput is one use_skill on a skill outside the agent's effective set.
type RequestInput struct {
	AgentID   string
	Skill     string
	RunID     string
	Rationale string
}

// Outcome is a request's answer: Access is domain.SkillAccessGranted,
// SkillAccessPending or SkillAccessDenied; Detail is shown to the agent.
type Outcome struct {
	Access string
	Detail string
}

// Request asks for access and waits up to Config.RequestTimeout for an operator.
// Approved → a durable grant (source "request") and Access granted, so the caller
// serves the body in the same run. No decision in time → pending: the request
// stays open for operators, and a later grant applies to future runs. Denied →
// denied. Never returns an error: the agent gets an observation either way.
func (s *Service) Request(ctx context.Context, in RequestInput) Outcome {
	if s == nil || s.store == nil {
		return Outcome{Access: domain.SkillAccessDenied,
			Detail: "this kernel keeps no skill grant store (migration 0031), so access cannot be granted"}
	}
	if in.AgentID == "" || in.Skill == "" {
		return Outcome{Access: domain.SkillAccessDenied, Detail: "the request names no agent or skill"}
	}
	now := s.now().UTC()
	if s.cfg.AutoApprove {
		if _, err := s.store.Grant(ctx, domain.SkillGrant{
			AgentID: in.AgentID, SkillName: in.Skill, Source: domain.SkillGrantSourceRequest,
			GrantedBy: ApproverAuto, GrantedAt: now, Reason: "execution.skills.access_auto_approve",
		}, ""); err != nil {
			slog.ErrorContext(ctx, "skill access: auto-approved grant not recorded", "agent_id", in.AgentID, "skill", in.Skill, "err", err)
			return Outcome{Access: domain.SkillAccessDenied, Detail: "the grant could not be recorded"}
		}
		slog.InfoContext(ctx, "skill access: request auto-approved", "event", "skill_access_granted",
			"agent_id", in.AgentID, "skill", in.Skill, "run_id", in.RunID, "approver", ApproverAuto)
		return Outcome{Access: domain.SkillAccessGranted, Detail: "auto-approved"}
	}
	if err := s.store.OpenRequest(ctx, domain.SkillAccessRequest{
		AgentID: in.AgentID, SkillName: in.Skill, RunID: in.RunID, Rationale: in.Rationale, RequestedAt: now,
	}); err != nil {
		// Without a durable record a timeout would vanish; still ask live, but say so.
		slog.WarnContext(ctx, "skill access: request not recorded", "agent_id", in.AgentID, "skill", in.Skill, "err", err)
	}
	slog.InfoContext(ctx, "skill access: request raised", "event", "skill_access_requested",
		"agent_id", in.AgentID, "skill", in.Skill, "run_id", in.RunID)
	if s.approvals == nil {
		return pending()
	}
	preview := fmt.Sprintf("agent %s requests skill %s", in.AgentID, in.Skill)
	if in.RunID != "" {
		preview += " (run " + in.RunID + ")"
	}
	if r := strings.TrimSpace(in.Rationale); r != "" {
		preview += ": " + r
	}
	dec, err := s.approvals.Request(ctx, domain.ApprovalRequest{
		AgentID: in.AgentID, ToolName: "skill:" + in.Skill, ArgsPreview: preview,
		Kind: domain.ApprovalKindSkillAccess, Timeout: s.cfg.RequestTimeout,
	})
	switch {
	case err == nil && dec.Approved:
		by := dec.ApproverID
		if by == "" {
			by = "operator"
		}
		// WithoutCancel: the operator approved; a caller that gave up meanwhile must
		// not lose the durable grant the approval promised.
		if _, gerr := s.store.Grant(context.WithoutCancel(ctx), domain.SkillGrant{
			AgentID: in.AgentID, SkillName: in.Skill, Source: domain.SkillGrantSourceRequest,
			GrantedBy: by, GrantedAt: s.now().UTC(), Reason: "approved request",
		}, ""); gerr != nil {
			slog.ErrorContext(ctx, "skill access: approved grant not recorded", "agent_id", in.AgentID, "skill", in.Skill, "err", gerr)
			return Outcome{Access: domain.SkillAccessPending, Detail: "approved, but the grant could not be recorded; ask an operator to grant it"}
		}
		slog.InfoContext(ctx, "skill access: request approved", "event", "skill_access_granted",
			"agent_id", in.AgentID, "skill", in.Skill, "run_id", in.RunID, "approver", by)
		return Outcome{Access: domain.SkillAccessGranted, Detail: "approved by " + by}
	case err == nil && dec.Decided:
		by := dec.ApproverID
		if by == "" {
			by = "operator"
		}
		if derr := s.store.DenyRequest(context.WithoutCancel(ctx), in.AgentID, in.Skill, by, s.now().UTC()); derr != nil {
			slog.WarnContext(ctx, "skill access: denial not recorded", "agent_id", in.AgentID, "skill", in.Skill, "err", derr)
		}
		slog.InfoContext(ctx, "skill access: request denied", "event", "skill_access_denied",
			"agent_id", in.AgentID, "skill", in.Skill, "run_id", in.RunID, "approver", by)
		return Outcome{Access: domain.SkillAccessDenied, Detail: "an operator denied access"}
	}
	return pending()
}

func pending() Outcome {
	return Outcome{Access: domain.SkillAccessPending,
		Detail: "no operator decision yet; the request stays open, and an approval applies to later runs"}
}

// GrantInput is an operator's or the lifecycle's direct grant.
type GrantInput struct {
	CommandID string // operator commands only ("" otherwise)
	AgentID   string
	Skill     string
	Source    string // domain.SkillGrantSource*
	By        string
	Reason    string
}

// Result is what a grant or revoke command returns.
type Result struct {
	Changed bool // false: already in that state (no audit row)
	Deduped bool // the command_id already ran
}

// Grant makes a grant active (idempotent) and resolves the pair's open request.
func (s *Service) Grant(ctx context.Context, in GrantInput) (Result, error) {
	if s == nil || s.store == nil {
		return Result{}, fmt.Errorf("%w: no skill grant store (migration 0031)", ErrFailedPrecondition)
	}
	if in.AgentID == "" || in.Skill == "" {
		return Result{}, fmt.Errorf("%w: agent_id and skill_name are required", ErrInvalidArgument)
	}
	switch in.Source {
	case domain.SkillGrantSourceRequest, domain.SkillGrantSourceProposal, domain.SkillGrantSourceOperator:
	default:
		return Result{}, fmt.Errorf("%w: unknown grant source %q", ErrInvalidArgument, in.Source)
	}
	if r, ok, err := s.deduped(ctx, in.CommandID); err != nil || ok {
		return r, err
	}
	if in.Source == domain.SkillGrantSourceOperator && s.known != nil && !s.known(in.Skill) {
		return Result{}, fmt.Errorf("%w: the kernel serves no skill named %s", ErrFailedPrecondition, in.Skill)
	}
	changed, err := s.store.Grant(ctx, domain.SkillGrant{
		AgentID: in.AgentID, SkillName: in.Skill, Source: in.Source, GrantedBy: in.By,
		GrantedAt: s.now().UTC(), Reason: in.Reason,
	}, in.CommandID)
	if err != nil {
		return Result{}, err
	}
	slog.InfoContext(ctx, "skill access: granted", "event", "skill_grant",
		"agent_id", in.AgentID, "skill", in.Skill, "source", in.Source, "by", in.By, "changed", changed)
	return Result{Changed: changed}, nil
}

// RevokeInput is an operator's revoke.
type RevokeInput struct {
	CommandID string
	AgentID   string
	Skill     string
	By        string
	Reason    string
}

// Revoke ends an active grant. A profile skill cannot be revoked here (it is
// removed by editing the profile); revoking a pair with no active grant is
// ErrNotFound.
func (s *Service) Revoke(ctx context.Context, in RevokeInput) (Result, error) {
	if s == nil || s.store == nil {
		return Result{}, fmt.Errorf("%w: no skill grant store (migration 0031)", ErrFailedPrecondition)
	}
	if in.AgentID == "" || in.Skill == "" {
		return Result{}, fmt.Errorf("%w: agent_id and skill_name are required", ErrInvalidArgument)
	}
	if r, ok, err := s.deduped(ctx, in.CommandID); err != nil || ok {
		return r, err
	}
	if err := s.store.Revoke(ctx, in.AgentID, in.Skill, in.By, in.Reason, in.CommandID, s.now().UTC()); err != nil {
		if errors.Is(err, domain.ErrSkillGrantNotFound) {
			return Result{}, fmt.Errorf("%w: %s has no active grant of %s", ErrNotFound, in.AgentID, in.Skill)
		}
		return Result{}, err
	}
	if own, bound := s.profiles(in.AgentID); bound {
		for _, n := range own {
			if n == in.Skill {
				slog.WarnContext(ctx, "skill access: revoked a grant of a skill the agent's profile also lists; it keeps the skill through its profile",
					"agent_id", in.AgentID, "skill", in.Skill)
			}
		}
	}
	slog.InfoContext(ctx, "skill access: revoked", "event", "skill_grant_revoked",
		"agent_id", in.AgentID, "skill", in.Skill, "by", in.By)
	return Result{Changed: true}, nil
}

func (s *Service) deduped(ctx context.Context, commandID string) (Result, bool, error) {
	if commandID == "" {
		return Result{}, false, nil
	}
	_, ok, err := s.store.EventByCommand(ctx, commandID)
	if err != nil || !ok {
		return Result{}, false, err
	}
	return Result{Deduped: true}, true, nil
}

// Grants lists grants (active only unless f.IncludeRevoked).
func (s *Service) Grants(ctx context.Context, f domain.SkillGrantFilter) ([]domain.SkillGrant, error) {
	if s == nil || s.store == nil {
		return nil, fmt.Errorf("%w: no skill grant store (migration 0031)", ErrFailedPrecondition)
	}
	return s.store.ListGrants(ctx, f)
}

// Requests lists access requests (f.State "" = all).
func (s *Service) Requests(ctx context.Context, f domain.SkillGrantFilter) ([]domain.SkillAccessRequest, error) {
	if s == nil || s.store == nil {
		return nil, fmt.Errorf("%w: no skill grant store (migration 0031)", ErrFailedPrecondition)
	}
	return s.store.ListRequests(ctx, f)
}

// GrantProposer is the D13 hook: approving a skill proposal grants the skill to
// the agent that proposed it (decisions/skill-access-grants.md, refinement b).
// It satisfies lifecycle.ProposalGranter.
func (s *Service) GrantProposer(ctx context.Context, agentID, skill, approver, proposalID string) error {
	_, err := s.Grant(ctx, GrantInput{
		AgentID: agentID, Skill: skill, Source: domain.SkillGrantSourceProposal,
		By: approver, Reason: "approved proposal " + proposalID,
	})
	return err
}
