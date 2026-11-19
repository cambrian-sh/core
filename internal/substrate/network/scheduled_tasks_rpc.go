package network

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/scheduledtask"
)

// Principal-owned recurring tasks on the agent plane
// (T-20261106-the-assistant-cannot-set-up-a-recurring-task). Every RPC resolves
// the OWNER from the presented lease through RunPrincipal — the one lease →
// principal resolution (conversation owner → task beneficiary → configured
// default). Nothing in a request can name whose schedule is created, listed or
// changed (INV-5): an agent can only ever act on the schedules of the person its
// run is for.

// scheduleCaller resolves the presented lease to (owner, proposing agent).
func (s *Server) scheduleCaller(ctx context.Context, leaseField string) (owner, agentID string, err error) {
	if s.ScheduledTasks == nil {
		return "", "", status.Error(codes.Unimplemented, "scheduled tasks are not enabled on this kernel (capability scheduled-tasks)")
	}
	lease := domain.LeaseID(leaseField)
	if lease == "" {
		lease = domain.LeaseID(mdValue(ctx, leaseHeader))
	}
	if lease == "" {
		return "", "", status.Error(codes.PermissionDenied, "a lease is required")
	}
	b, ok := s.resolveLeaseFor(ctx, lease)
	if !ok {
		return "", "", status.Error(codes.PermissionDenied, "lease is missing, foreign or expired")
	}
	agentID = b.AgentID
	if agentID == "" {
		agentID, _ = callerAgentIdentity(ctx)
	}
	if s.RunPrincipal != nil {
		owner = s.RunPrincipal(ctx, b)
	}
	return owner, agentID, nil
}

var scheduleRejectionToPB = map[scheduledtask.Rejection]pb.ScheduledTaskRejection{
	scheduledtask.RejectNone:        pb.ScheduledTaskRejection_SCHEDULED_TASK_REJECTION_UNSPECIFIED,
	scheduledtask.RejectInvalid:     pb.ScheduledTaskRejection_SCHEDULED_TASK_REJECTION_INVALID,
	scheduledtask.RejectLimit:       pb.ScheduledTaskRejection_SCHEDULED_TASK_REJECTION_LIMIT,
	scheduledtask.RejectTooFrequent: pb.ScheduledTaskRejection_SCHEDULED_TASK_REJECTION_TOO_FREQUENT,
	scheduledtask.RejectDenied:      pb.ScheduledTaskRejection_SCHEDULED_TASK_REJECTION_DENIED,
	scheduledtask.RejectNoDecision:  pb.ScheduledTaskRejection_SCHEDULED_TASK_REJECTION_NO_DECISION,
	scheduledtask.RejectNoPrincipal: pb.ScheduledTaskRejection_SCHEDULED_TASK_REJECTION_NO_PRINCIPAL,
}

// ScheduledTaskToPB converts a task for the agent plane.
func ScheduledTaskToPB(t domain.ScheduledTask) *pb.ScheduledTask {
	created := ""
	if !t.CreatedAt.IsZero() {
		created = t.CreatedAt.UTC().Format(time.RFC3339)
	}
	return &pb.ScheduledTask{
		Id: t.ID, OwnerId: t.OwnerID, Title: t.Title, Goal: t.Goal, Cron: t.Cron,
		Timezone: t.Timezone, AgentId: t.AgentID, Delivery: t.Delivery, Active: t.Active,
		ConversationId: t.ConversationID, CreatedAt: created,
	}
}

// ProposeScheduledTask asks the owner's approval and creates the task only on an
// explicit approve. A refusal is an OK response with created=false and a
// rejection — the shape a tool result can show the model.
//
// It BLOCKS while the approval is pending (bounded by
// execution.schedules.approval_timeout_seconds), like a dangerous-tool approval:
// the agent learns the outcome in the same step and can tell the user.
func (s *Server) ProposeScheduledTask(ctx context.Context, req *pb.ProposeScheduledTaskRequest) (*pb.ProposeScheduledTaskResponse, error) {
	owner, agentID, err := s.scheduleCaller(ctx, req.GetLeaseId())
	if err != nil {
		return nil, err
	}
	// The proposal blocks on a person for up to the approval window. Admitted
	// like a tool call (admitTurnWork): the wait counts as the chat turn waiting
	// on a person, not as silence, and the turn's end withdraws the approval, so
	// a late approve is refused rather than creating a task for a turn whose
	// person was already answered (T-20261118-a-chat-turn-waiting-on-a-human-
	// approval-is).
	lease := domain.LeaseID(req.GetLeaseId())
	if lease == "" {
		lease = domain.LeaseID(mdValue(ctx, leaseHeader))
	}
	ctx, release, ok := s.admitTurnWork(ctx, lease)
	if !ok {
		return &pb.ProposeScheduledTaskResponse{
			Rejection: pb.ScheduledTaskRejection_SCHEDULED_TASK_REJECTION_NO_DECISION,
			Detail:    ErrTurnEnded.Error() + "; nothing was created",
		}, nil
	}
	defer release()
	// ENG-2247: the proposal raises its own approval; from a session the Rule of
	// Two stops it carries the "Rule of Two:" notice, so the person sees why
	// without being asked twice.
	notice := s.ruleOfTwoNotice(ctx, req.GetLeaseId(), "schedule_task", "schedule "+req.GetTitle(),
		map[string]any{"title": req.GetTitle(), "goal": req.GetGoal(), "cron": req.GetCron(), "rrule": req.GetRrule()})
	out, err := s.ScheduledTasks.Propose(ctx, scheduledtask.Proposal{
		OwnerID: owner, AgentID: agentID, Title: req.GetTitle(), Goal: req.GetGoal(),
		Cron: req.GetCron(), RRule: req.GetRrule(), Timezone: req.GetTimezone(),
		RunAgent: req.GetAgentId(), Delivery: req.GetDelivery(), Notice: notice,
	})
	if err != nil && errors.Is(context.Cause(ctx), ErrTurnEnded) {
		// The chat turn that asked ended while the approval was pending: the hub
		// withdrew it, nothing was created, and nobody is waiting for this answer.
		slog.InfoContext(ctx, "scheduled task: proposal withdrawn; its chat turn ended", "owner", owner, "agent", agentID)
		return &pb.ProposeScheduledTaskResponse{
			Rejection: pb.ScheduledTaskRejection_SCHEDULED_TASK_REJECTION_NO_DECISION,
			Detail:    ErrTurnEnded.Error() + "; nothing was created",
		}, nil
	}
	if err != nil {
		slog.WarnContext(ctx, "scheduled task: proposal failed", "owner", owner, "agent", agentID, "err", err)
		return nil, status.Error(codes.Unavailable, "scheduled tasks are unavailable")
	}
	resp := &pb.ProposeScheduledTaskResponse{
		Created: out.Created, Rejection: scheduleRejectionToPB[out.Rejection], Detail: out.Detail,
	}
	if out.Created {
		resp.Task = ScheduledTaskToPB(out.Task)
		slog.InfoContext(ctx, "scheduled task: created on approval", "id", out.Task.ID,
			"owner", owner, "agent", agentID, "cron", out.Task.Cron, "approved_by", out.Task.ApprovedBy)
	}
	return resp, nil
}

// ListScheduledTasks returns the run principal's own tasks.
func (s *Server) ListScheduledTasks(ctx context.Context, req *pb.ListScheduledTasksRequest) (*pb.ListScheduledTasksResponse, error) {
	owner, _, err := s.scheduleCaller(ctx, req.GetLeaseId())
	if err != nil {
		return nil, err
	}
	if owner == "" {
		return &pb.ListScheduledTasksResponse{}, nil
	}
	tasks, err := s.ScheduledTasks.List(ctx, owner)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "scheduled tasks are unavailable")
	}
	resp := &pb.ListScheduledTasksResponse{}
	for _, t := range tasks {
		resp.Tasks = append(resp.Tasks, ScheduledTaskToPB(t))
	}
	return resp, nil
}

// ManageScheduledTask pauses, resumes or deletes one of the run principal's own
// tasks. Someone else's task (or none) is ok=false "not found" — never a hint that
// it exists.
func (s *Server) ManageScheduledTask(ctx context.Context, req *pb.ManageScheduledTaskRequest) (*pb.ManageScheduledTaskResponse, error) {
	owner, _, err := s.scheduleCaller(ctx, req.GetLeaseId())
	if err != nil {
		return nil, err
	}
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	// ENG-2247: pausing, resuming or deleting a recurring task is a write; from a
	// tainted session it asks the person first (the Rule of Two).
	if why, ok := s.gateKernelWrite(ctx, req.GetLeaseId(), "manage_scheduled_task",
		req.GetAction().String()+" scheduled task "+req.GetId(),
		map[string]any{"id": req.GetId(), "action": req.GetAction().String()}); !ok {
		return &pb.ManageScheduledTaskResponse{Detail: why}, nil
	}
	switch req.GetAction() {
	case pb.ScheduledTaskAction_SCHEDULED_TASK_ACTION_PAUSE:
		err = s.ScheduledTasks.SetActive(ctx, owner, req.GetId(), false)
	case pb.ScheduledTaskAction_SCHEDULED_TASK_ACTION_RESUME:
		err = s.ScheduledTasks.SetActive(ctx, owner, req.GetId(), true)
	case pb.ScheduledTaskAction_SCHEDULED_TASK_ACTION_DELETE:
		err = s.ScheduledTasks.Delete(ctx, owner, req.GetId())
	default:
		return nil, status.Error(codes.InvalidArgument, "action must be pause, resume or delete")
	}
	if errors.Is(err, domain.ErrScheduleNotFound) {
		return &pb.ManageScheduledTaskResponse{Detail: "no such scheduled task"}, nil
	}
	if err != nil {
		return nil, status.Error(codes.Unavailable, "scheduled tasks are unavailable")
	}
	slog.InfoContext(ctx, "scheduled task: changed by its owner's assistant",
		"id", req.GetId(), "owner", owner, "action", req.GetAction().String())
	return &pb.ManageScheduledTaskResponse{Ok: true}, nil
}
