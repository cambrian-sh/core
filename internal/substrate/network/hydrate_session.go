package network

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
)

// HydrateSession loads the latest checkpoint + associated plan for resuming execution.
// Returns the checkpoint context, the plan, and the next step index to execute.
// Returns nil context if no checkpoint exists (fresh session).
func (s *Server) HydrateSession(ctx context.Context, sessionID string) (map[string]string, *domain.ExecutionPlan, int, error) {
	store := s.executorCheckpointStore()
	if store == nil {
		return nil, nil, 0, nil
	}

	metas, err := store.ListCheckpoints(sessionID)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("list checkpoints: %w", err)
	}
	if len(metas) == 0 {
		return nil, nil, 0, nil
	}

	latest := metas[len(metas)-1]
	slog.Info("HydrateSession: resuming from checkpoint", "session", sessionID, "plan", latest.PlanID, "step", latest.StepIndex)

	ctxMap, err := store.LoadCheckpoint(latest.SessionID, latest.PlanID, latest.StepIndex)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("load checkpoint: %w", err)
	}

	startFrom := latest.StepIndex + 1
	return ctxMap, nil, startFrom, nil
}

func (s *Server) executorCheckpointStore() executer.CheckpointStore {
	if reg, ok := s.Manager.Registry.(executer.CheckpointStore); ok {
		return reg
	}
	return nil
}
