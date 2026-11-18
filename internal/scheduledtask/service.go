// Package scheduledtask is the kernel side of principal-owned recurring tasks
// (T-20261106-the-assistant-cannot-set-up-a-recurring-task): an assistant proposes
// a task for the principal it serves, the principal approves it through the one
// approval hub (kind "schedule" — the console Inbox), and only then is it created
// in the premium schedule store (domain.PrincipalSchedules), owned by that
// principal.
//
// The kernel owns the GATE (who the owner is, the approval, the caps it passes
// down); the premium reactive plugin owns the SCHEDULE (a schedule watch on the one
// scheduler, ReactiveEngine). Nothing here runs a schedule.
package scheduledtask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// Rejection says why a proposal created nothing. The agent plane maps it 1:1 onto
// ScheduledTaskRejection so the model sees a reason it can relay.
type Rejection int

const (
	RejectNone Rejection = iota
	RejectInvalid
	RejectLimit
	RejectTooFrequent
	RejectDenied
	RejectNoDecision
	RejectNoPrincipal
)

// MaxGoalChars bounds a task's goal: it is replayed verbatim as a chat turn at
// every firing, so an unbounded one is an unbounded recurring prompt.
const MaxGoalChars = 4000

// Proposal is one agent's request. OwnerID comes from the lease (never the wire).
type Proposal struct {
	OwnerID  string `json:"owner_id"`
	AgentID  string `json:"agent_id,omitempty"` // the proposing agent
	Title    string `json:"title,omitempty"`
	Goal     string `json:"goal"`
	Cron     string `json:"cron,omitempty"`
	RRule    string `json:"rrule,omitempty"`
	Timezone string `json:"timezone,omitempty"`
	// RunAgent is the DEPRECATED agent_id of the proposal. It was recorded and
	// never routed — every firing runs as the owner on the chat lane — so a
	// non-empty value is refused rather than accepted and ignored (owner decision
	// 2026-11-15, ENG-753; the default single-agent lane, ADR-0150, has no
	// per-task agent choice).
	RunAgent string `json:"run_agent,omitempty"`
	Delivery string `json:"delivery,omitempty"`
}

// Outcome is a proposal's result. Created=false always carries a Rejection.
type Outcome struct {
	Created   bool
	Task      domain.ScheduledTask
	Rejection Rejection
	Detail    string
}

// Service gates proposals. Every field is required; New refuses a partial one so
// a service can never exist that would create without asking (AGENTS.md §2.3).
type Service struct {
	store           domain.PrincipalSchedules
	approvals       domain.ApprovalController
	limits          domain.ScheduleLimits
	approvalTimeout time.Duration
}

// New builds the service. approvals is the operator approval hub; a nil one is an
// error rather than a silently auto-approving service.
func New(store domain.PrincipalSchedules, approvals domain.ApprovalController,
	limits domain.ScheduleLimits, approvalTimeout time.Duration) (*Service, error) {
	if store == nil {
		return nil, errors.New("scheduledtask: a schedule store is required")
	}
	if approvals == nil {
		return nil, errors.New("scheduledtask: an approval channel is required (a schedule is never created silently)")
	}
	return &Service{store: store, approvals: approvals, limits: limits, approvalTimeout: approvalTimeout}, nil
}

// Limits reports the caps this service enforces.
func (s *Service) Limits() domain.ScheduleLimits { return s.limits }

// spec validates and normalises a proposal into the spec the store creates.
func spec(p Proposal) (domain.ScheduledTaskSpec, string) {
	if strings.TrimSpace(p.RunAgent) != "" {
		return domain.ScheduledTaskSpec{}, "agent_id is deprecated and must be empty: a scheduled task always runs as its owner on the chat lane"
	}
	goal := strings.TrimSpace(p.Goal)
	if goal == "" {
		return domain.ScheduledTaskSpec{}, "goal is required"
	}
	if len([]rune(goal)) > MaxGoalChars {
		return domain.ScheduledTaskSpec{}, fmt.Sprintf("goal is longer than %d characters", MaxGoalChars)
	}
	cron, rrule := strings.TrimSpace(p.Cron), strings.TrimSpace(p.RRule)
	switch {
	case cron != "" && rrule != "":
		return domain.ScheduledTaskSpec{}, "give exactly one of cron or rrule"
	case cron == "" && rrule == "":
		return domain.ScheduledTaskSpec{}, "a schedule (cron or rrule) is required"
	case rrule != "":
		c, err := domain.RRuleToCron(rrule)
		if err != nil {
			return domain.ScheduledTaskSpec{}, err.Error()
		}
		cron = c
	}
	delivery := strings.TrimSpace(p.Delivery)
	if !domain.ValidScheduleDelivery(delivery) {
		return domain.ScheduledTaskSpec{}, fmt.Sprintf("delivery %q must be conversation, notify or email", delivery)
	}
	if delivery == "" {
		delivery = domain.ScheduleDeliverConversation
	}
	title := strings.TrimSpace(p.Title)
	if title == "" {
		title = goal
		if r := []rune(title); len(r) > 60 {
			title = string(r[:60]) + "…"
		}
	}
	return domain.ScheduledTaskSpec{
		OwnerID: p.OwnerID, Title: title, Goal: goal, Cron: cron,
		Timezone: strings.TrimSpace(p.Timezone),
		Delivery: delivery, ProposedBy: p.AgentID,
	}, ""
}

// rejectionOf maps a store refusal onto a Rejection; ok=false for a real failure.
func rejectionOf(err error) (Rejection, bool) {
	switch {
	case errors.Is(err, domain.ErrScheduleInvalid):
		return RejectInvalid, true
	case errors.Is(err, domain.ErrScheduleLimit):
		return RejectLimit, true
	case errors.Is(err, domain.ErrScheduleTooFrequent):
		return RejectTooFrequent, true
	}
	return RejectNone, false
}

// Propose validates the proposal and the caps, asks the owner's approval, and
// creates the task only on an explicit approve. Refusals are an Outcome, not an
// error; an error is a store or channel failure.
func (s *Service) Propose(ctx context.Context, p Proposal) (Outcome, error) {
	if strings.TrimSpace(p.OwnerID) == "" {
		return Outcome{Rejection: RejectNoPrincipal,
			Detail: "this run is not acting for a person, so there is nobody to own a schedule"}, nil
	}
	sp, bad := spec(p)
	if bad != "" {
		return Outcome{Rejection: RejectInvalid, Detail: bad}, nil
	}
	// Caps and cron validity BEFORE the user is asked: an approval for something
	// that cannot be created wastes the person's attention.
	if err := s.store.Check(ctx, sp, s.limits); err != nil {
		if r, ok := rejectionOf(err); ok {
			return Outcome{Rejection: r, Detail: err.Error()}, nil
		}
		return Outcome{}, fmt.Errorf("scheduledtask: check: %w", err)
	}

	dec, err := s.approvals.Request(ctx, domain.ApprovalRequest{
		AgentID:     p.AgentID,
		ToolName:    "schedule_task",
		ArgsPreview: Preview(sp),
		Kind:        domain.ApprovalKindSchedule,
		Timeout:     s.approvalTimeout,
		// ADR-0142 D11: a person's own recurring task is their own resource, so
		// the default policy lets them (or an operator) approve it.
		Beneficiary: domain.PersonPrincipal(p.OwnerID),
		// What to create if the approve lands after this kernel run ended
		// (T-20261118-a-sync-approval-orphaned-by-a-kernel-restart): the
		// proposal as validated, owned by the lease's person.
		Intent: proposalIntent(p),
	})
	if err != nil {
		return Outcome{}, fmt.Errorf("scheduledtask: approval: %w", err)
	}
	switch {
	case !dec.Decided:
		return Outcome{Rejection: RejectNoDecision,
			Detail: "nobody approved the schedule in time; nothing was created"}, nil
	case !dec.Approved:
		return Outcome{Rejection: RejectDenied, Detail: "the schedule was declined; nothing was created"}, nil
	}
	sp.ApprovedBy = dec.ApproverID
	task, err := s.store.Create(ctx, sp, s.limits)
	if err != nil {
		// A cap can be reached between Check and Create (a concurrent proposal).
		if r, ok := rejectionOf(err); ok {
			return Outcome{Rejection: r, Detail: err.Error()}, nil
		}
		return Outcome{}, fmt.Errorf("scheduledtask: create: %w", err)
	}
	return Outcome{Created: true, Task: task}, nil
}

// List returns the owner's tasks. An empty owner is refused: an agent-plane caller
// never lists everyone's (that is the operator view).
func (s *Service) List(ctx context.Context, ownerID string) ([]domain.ScheduledTask, error) {
	if strings.TrimSpace(ownerID) == "" {
		return nil, domain.ErrScheduleNotFound
	}
	return s.store.List(ctx, ownerID)
}

// SetActive pauses/resumes one of the owner's tasks.
func (s *Service) SetActive(ctx context.Context, ownerID, id string, active bool) error {
	if strings.TrimSpace(ownerID) == "" {
		return domain.ErrScheduleNotFound
	}
	return s.store.SetActive(ctx, ownerID, id, active)
}

// Delete removes one of the owner's tasks.
func (s *Service) Delete(ctx context.Context, ownerID, id string) error {
	if strings.TrimSpace(ownerID) == "" {
		return domain.ErrScheduleNotFound
	}
	return s.store.Delete(ctx, ownerID, id)
}

// Preview is the one-line description an approver reads: WHOSE task first, then
// when and what. It is what the console Inbox renders as the request's arguments.
func Preview(sp domain.ScheduledTaskSpec) string {
	tz := sp.Timezone
	if tz == "" {
		tz = "UTC"
	}
	goal := sp.Goal
	if r := []rune(goal); len(r) > 240 {
		goal = string(r[:240]) + "…"
	}
	return fmt.Sprintf("for %s: %q — cron %q (%s), delivery %s: %s",
		sp.OwnerID, sp.Title, sp.Cron, tz, sp.Delivery, goal)
}

// ── approvals answered after a restart ──────────────────────────────────────

// proposalIntent is the intent a schedule approval stores: the proposal, so an
// approve that lands after the proposing kernel run ended still creates the
// task (Service.ApprovalRecoverer).
func proposalIntent(p Proposal) *domain.ApprovalIntent {
	args, err := json.Marshal(p)
	if err != nil {
		return nil // a Proposal of strings always marshals; nil refuses after a restart
	}
	return &domain.ApprovalIntent{
		Executor: domain.ApprovalIntentSchedule, AgentID: p.AgentID, Tool: "schedule_task",
		Args: args, OnBehalfOf: p.OwnerID,
	}
}

// ApprovalRecoverer returns the recoverer of schedule intents
// (domain.ApprovalIntentSchedule) for AddressedHub.RecoverWith: on an approve
// that lands after the proposing kernel run ended, the task is created for its
// owner — once (the hub's ADR-0154 action record), only while the owner is a
// known, active principal (directory; nil refuses, as a task's firing does) and
// only within the caps in force now.
func (s *Service) ApprovalRecoverer(directory func() domain.PrincipalDirectory) domain.ApprovalRecoverer {
	return &scheduleRecoverer{s: s, directory: directory}
}

type scheduleRecoverer struct {
	s         *Service
	directory func() domain.PrincipalDirectory
}

func (r *scheduleRecoverer) spec(ctx context.Context, it domain.ApprovalItem) (domain.ScheduledTaskSpec, error) {
	in := it.Intent
	if in == nil || in.Executor != domain.ApprovalIntentSchedule {
		return domain.ScheduledTaskSpec{}, errors.New("the approval does not name a schedule")
	}
	var p Proposal
	if err := json.Unmarshal(in.Args, &p); err != nil {
		return domain.ScheduledTaskSpec{}, fmt.Errorf("the stored proposal is unreadable: %v", err)
	}
	if strings.TrimSpace(p.OwnerID) == "" || p.OwnerID != in.OnBehalfOf {
		return domain.ScheduledTaskSpec{}, errors.New("the stored proposal names no owner")
	}
	var dir domain.PrincipalDirectory
	if r.directory != nil {
		dir = r.directory()
	}
	if dir == nil {
		return domain.ScheduledTaskSpec{}, fmt.Errorf("nothing can confirm that %s is still an active person", p.OwnerID)
	}
	if known, err := dir.KnownPrincipal(ctx, p.OwnerID); err != nil || !known {
		return domain.ScheduledTaskSpec{}, fmt.Errorf("%s is no longer a known, active person", p.OwnerID)
	}
	sp, bad := spec(p)
	if bad != "" {
		return domain.ScheduledTaskSpec{}, errors.New(bad)
	}
	return sp, nil
}

// CheckRecovery: the owner is known and active, the proposal is still valid and
// fits the owner's caps now.
func (r *scheduleRecoverer) CheckRecovery(ctx context.Context, it domain.ApprovalItem) error {
	sp, err := r.spec(ctx, it)
	if err != nil {
		return err
	}
	if err := r.s.store.Check(ctx, sp, r.s.limits); err != nil {
		return err
	}
	return nil
}

// Recover creates the approved task.
func (r *scheduleRecoverer) Recover(ctx context.Context, it domain.ApprovalItem) (string, error) {
	sp, err := r.spec(ctx, it)
	if err != nil {
		return "", fmt.Errorf("%w: %v", domain.ErrRecoveryRefused, err)
	}
	sp.ApprovedBy = it.ResolvedBy
	task, err := r.s.store.Create(ctx, sp, r.s.limits)
	if err != nil {
		if _, refused := rejectionOf(err); refused {
			return "", fmt.Errorf("%w: %v", domain.ErrRecoveryRefused, err)
		}
		return "", fmt.Errorf("creating the scheduled task failed: %w", err)
	}
	return domain.RecoverySummary(fmt.Sprintf("created scheduled task %s (%q, cron %q)", task.ID, task.Title, task.Cron)), nil
}
