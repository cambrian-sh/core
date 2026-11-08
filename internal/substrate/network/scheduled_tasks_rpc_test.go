package network

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/scheduledtask"
)

// Principal-owned recurring tasks on the agent plane: the OWNER is always the run
// principal of the presented lease, the user approves through the hub, and an
// agent can only list/change the schedules of the person its run is for.

type memSchedules struct {
	mu    sync.Mutex
	tasks map[string]domain.ScheduledTask
}

func (m *memSchedules) Check(context.Context, domain.ScheduledTaskSpec, domain.ScheduleLimits) error {
	return nil
}

func (m *memSchedules) Create(_ context.Context, sp domain.ScheduledTaskSpec, _ domain.ScheduleLimits) (domain.ScheduledTask, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := domain.ScheduledTaskID(sp)
	t := domain.ScheduledTask{ID: id, OwnerID: sp.OwnerID, Goal: sp.Goal, Cron: sp.Cron, Active: true, ApprovedBy: sp.ApprovedBy}
	m.tasks[id] = t
	return t, nil
}

func (m *memSchedules) List(_ context.Context, owner string) ([]domain.ScheduledTask, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.ScheduledTask
	for _, t := range m.tasks {
		if owner == "" || t.OwnerID == owner {
			out = append(out, t)
		}
	}
	return out, nil
}

func (m *memSchedules) SetActive(_ context.Context, owner, id string, active bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok || (owner != "" && t.OwnerID != owner) {
		return domain.ErrScheduleNotFound
	}
	t.Active = active
	m.tasks[id] = t
	return nil
}

func (m *memSchedules) Delete(_ context.Context, owner, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok || (owner != "" && t.OwnerID != owner) {
		return domain.ErrScheduleNotFound
	}
	delete(m.tasks, id)
	return nil
}

type decideAll struct{ approve bool }

func (d decideAll) Request(context.Context, domain.ApprovalRequest) (domain.ApprovalDecision, error) {
	return domain.ApprovalDecision{Approved: d.approve, Decided: true, ApproverID: "the-user"}, nil
}

func scheduleServer(t *testing.T, approve bool) (*Server, *SubstrateLLMGateway, *memSchedules) {
	t.Helper()
	s, gw := newLeaseServer(t)
	store := &memSchedules{tasks: map[string]domain.ScheduledTask{}}
	svc, err := scheduledtask.New(store, decideAll{approve: approve}, domain.ScheduleLimits{}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	s.ScheduledTasks = svc
	// The run principal travels on the binding's conversation in this fixture.
	s.RunPrincipal = func(_ context.Context, b domain.LeaseBinding) string {
		return strings.TrimPrefix(b.ConversationID, "conv-of-")
	}
	return s, gw, store
}

func TestScheduledTaskRPC_OwnerIsTheLeasesPrincipal(t *testing.T) {
	t.Parallel()
	s, gw, store := scheduleServer(t, true)
	lease := bindNotesLease(t, gw, domain.LeaseBinding{AgentID: "assistant", ConversationID: "conv-of-lena"})
	ctx := agentPlaneCtx("assistant")

	resp, err := s.ProposeScheduledTask(ctx, &pb.ProposeScheduledTaskRequest{LeaseId: string(lease),
		Goal: "weekly summary", Rrule: "FREQ=WEEKLY;BYDAY=MO;BYHOUR=8"})
	if err != nil || !resp.GetCreated() {
		t.Fatalf("propose: %+v %v", resp, err)
	}
	if resp.GetTask().GetOwnerId() != "lena" || resp.GetTask().GetCron() != "0 8 * * 1" {
		t.Fatalf("task = %+v (the owner must be the lease's run principal)", resp.GetTask())
	}

	// Dana's assistant cannot see or touch Lena's task.
	dana := bindNotesLease(t, gw, domain.LeaseBinding{AgentID: "assistant", ConversationID: "conv-of-dana"})
	l, _ := s.ListScheduledTasks(ctx, &pb.ListScheduledTasksRequest{LeaseId: string(dana)})
	if len(l.GetTasks()) != 0 {
		t.Fatalf("dana listed lena's tasks: %+v", l)
	}
	m, err := s.ManageScheduledTask(ctx, &pb.ManageScheduledTaskRequest{LeaseId: string(dana),
		Id: resp.GetTask().GetId(), Action: pb.ScheduledTaskAction_SCHEDULED_TASK_ACTION_DELETE})
	if err != nil || m.GetOk() {
		t.Fatalf("dana deleted lena's task: %+v %v", m, err)
	}

	// Lena's assistant pauses, resumes, deletes.
	for _, a := range []pb.ScheduledTaskAction{
		pb.ScheduledTaskAction_SCHEDULED_TASK_ACTION_PAUSE,
		pb.ScheduledTaskAction_SCHEDULED_TASK_ACTION_RESUME,
		pb.ScheduledTaskAction_SCHEDULED_TASK_ACTION_DELETE,
	} {
		m, err := s.ManageScheduledTask(ctx, &pb.ManageScheduledTaskRequest{LeaseId: string(lease), Id: resp.GetTask().GetId(), Action: a})
		if err != nil || !m.GetOk() {
			t.Fatalf("%v: %+v %v", a, m, err)
		}
	}
	if len(store.tasks) != 0 {
		t.Fatalf("delete left %+v", store.tasks)
	}
}

func TestScheduledTaskRPC_DeniedCreatesNothing(t *testing.T) {
	t.Parallel()
	s, gw, store := scheduleServer(t, false)
	lease := bindNotesLease(t, gw, domain.LeaseBinding{AgentID: "assistant", ConversationID: "conv-of-lena"})
	resp, err := s.ProposeScheduledTask(agentPlaneCtx("assistant"), &pb.ProposeScheduledTaskRequest{LeaseId: string(lease),
		Goal: "x", Cron: "@daily"})
	if err != nil || resp.GetCreated() || resp.GetRejection() != pb.ScheduledTaskRejection_SCHEDULED_TASK_REJECTION_DENIED {
		t.Fatalf("denied: %+v %v", resp, err)
	}
	if len(store.tasks) != 0 {
		t.Fatal("a denied proposal created a task")
	}
}

func TestScheduledTaskRPC_RefusalsAndUnwired(t *testing.T) {
	t.Parallel()
	s, gw, _ := scheduleServer(t, true)
	ctx := agentPlaneCtx("assistant")
	if _, err := s.ProposeScheduledTask(ctx, &pb.ProposeScheduledTaskRequest{Goal: "x", Cron: "@daily"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("no lease: %v", err)
	}
	// A lease bound to another agent confers nothing (ADR-0139 D8).
	foreign := bindNotesLease(t, gw, domain.LeaseBinding{AgentID: "other", ConversationID: "conv-of-lena"})
	if _, err := s.ProposeScheduledTask(ctx, &pb.ProposeScheduledTaskRequest{LeaseId: string(foreign), Goal: "x", Cron: "@daily"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("foreign lease: %v", err)
	}
	// A run acting for nobody has nobody to own a schedule.
	nobody := bindNotesLease(t, gw, domain.LeaseBinding{AgentID: "assistant"})
	resp, err := s.ProposeScheduledTask(ctx, &pb.ProposeScheduledTaskRequest{LeaseId: string(nobody), Goal: "x", Cron: "@daily"})
	if err != nil || resp.GetRejection() != pb.ScheduledTaskRejection_SCHEDULED_TASK_REJECTION_NO_PRINCIPAL {
		t.Fatalf("no principal: %+v %v", resp, err)
	}
	bare, _ := newLeaseServer(t)
	if _, err := bare.ListScheduledTasks(ctx, &pb.ListScheduledTasksRequest{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("unwired: %v", err)
	}
}

// The conversation owner is stamped for agent-plane reads from the LEASE, so the
// decision point can narrow the agent by what the owner may see.
func TestWithConversationOwner_FromLeaseOnly(t *testing.T) {
	t.Parallel()
	s, gw := newLeaseServer(t)
	s.ConversationOwner = func(_ context.Context, b domain.LeaseBinding) string {
		return strings.TrimPrefix(b.ConversationID, "conv-of-")
	}
	lease := bindNotesLease(t, gw, domain.LeaseBinding{AgentID: "chat_agent", ConversationID: "conv-of-dana"})
	ctx := s.withConversationOwner(agentPlaneCtx("chat_agent", leaseHeader, string(lease)))
	if got := domain.ConversationOwnerFromContext(ctx); got != "dana" {
		t.Fatalf("owner = %q, want dana", got)
	}
	// No lease: nothing stamped (the agent's own scope governs, as before).
	if got := domain.ConversationOwnerFromContext(s.withConversationOwner(agentPlaneCtx("chat_agent"))); got != "" {
		t.Fatalf("no lease stamped %q", got)
	}
	// Another agent presenting the lease gets nothing (D8).
	if got := domain.ConversationOwnerFromContext(s.withConversationOwner(agentPlaneCtx("intruder", leaseHeader, string(lease)))); got != "" {
		t.Fatalf("foreign lease stamped %q", got)
	}
	// And withCallerSession — the chokepoint every tool/content read takes — stamps it
	// even for a chat turn's lease, which has no session.
	if got := domain.ConversationOwnerFromContext(s.withCallerSession(agentPlaneCtx("chat_agent", leaseHeader, string(lease)))); got != "dana" {
		t.Fatalf("withCallerSession owner = %q", got)
	}
}
