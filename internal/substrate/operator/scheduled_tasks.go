package operator

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
)

// The operator view of principal-owned recurring tasks
// (T-20261106-the-assistant-cannot-set-up-a-recurring-task). Assistants propose
// them on the agent plane for the person they serve; operators see ALL of them
// here. Pausing and deleting reuse SetWatchActive / DeleteWatch on the same id —
// a scheduled task is a premium schedule watch, and a second set of mutation
// RPCs would be a second pathway to the same rows (AGENTS.md §4.3).

// ListScheduledTasks lists every principal's scheduled tasks, or one owner's.
// Operator-only (authz.go operatorOnlyReads): who scheduled what is other
// people's business.
func (s *Service) ListScheduledTasks(ctx context.Context, req *pb.ListScheduledTasksOpRequest) (*pb.ListScheduledTasksOpResponse, error) {
	if s.schedules == nil {
		return nil, status.Error(codes.Unimplemented, "scheduled tasks are not available on this kernel (capability scheduled-tasks)")
	}
	tasks, err := s.schedules.List(ctx, req.GetOwnerId())
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "scheduled tasks: %v", err)
	}
	out := &pb.ListScheduledTasksOpResponse{}
	for _, t := range tasks {
		created := ""
		if !t.CreatedAt.IsZero() {
			created = t.CreatedAt.UTC().Format(time.RFC3339)
		}
		out.Tasks = append(out.Tasks, &pb.ScheduledTaskOp{
			Id: t.ID, OwnerId: t.OwnerID, Title: t.Title, Goal: t.Goal, Cron: t.Cron,
			Timezone: t.Timezone, AgentId: t.AgentID, Delivery: t.Delivery, Active: t.Active,
			ConversationId: t.ConversationID, CreatedAt: created, ApprovedBy: t.ApprovedBy,
		})
	}
	return out, nil
}
