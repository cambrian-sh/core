package network

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
)

// ResumeMetadataKey names the run a request wants to continue. Resume is EXPLICIT: a caller
// asks for it by id, and gets that run's own persisted plan back.
//
// It replaces the implicit "hydrate on every Execute" behaviour, which was unsound. That
// path took the last checkpoint found anywhere in the session and applied its step index to
// a plan the planner had just generated for a NEW request — so steps 0..N of an unrelated
// plan were marked complete and silently skipped. Nothing in the request said "resume"; it
// happened whenever a session id happened to be present.
const ResumeMetadataKey = "_resume_run"

// ResumedRun is a run rehydrated for continuation.
type ResumedRun struct {
	Run *domain.Run
	// Plan is the run's OWN persisted plan, never a freshly generated one. That is what
	// makes StartFrom meaningful: the index points into the steps it was measured against.
	Plan *domain.ExecutionPlan
	// Context is the checkpointed master context to seed execution with.
	Context map[string]string
	// StartFrom is the index of the first step still to run.
	StartFrom int
}

// ResumeRun loads a run, its plan, and its latest checkpoint.
//
// Errors are returned rather than silently degrading to a fresh execution: a caller that
// asked to resume must not quietly get a brand-new run instead.
func (s *Server) ResumeRun(_ context.Context, runID domain.RunID, session domain.SessionID) (*ResumedRun, error) {
	if s.Runs == nil {
		return nil, fmt.Errorf("resume: no run store configured")
	}
	run, err := s.Runs.GetRun(runID)
	if err != nil {
		return nil, fmt.Errorf("resume: load run %q: %w", runID, err)
	}
	if run == nil {
		return nil, fmt.Errorf("resume: unknown run %q", runID)
	}
	// A run belongs to exactly one session; resuming it under another would splice one
	// session's work into another's history.
	if session != "" && run.SessionID != "" && run.SessionID != session {
		return nil, fmt.Errorf("resume: run %q belongs to session %q, not %q", runID, run.SessionID, session)
	}
	if !run.Resumable() {
		return nil, fmt.Errorf("resume: run %q is not resumable (status=%s, has_plan=%t)",
			runID, run.Status, run.Plan != nil)
	}

	out := &ResumedRun{Run: run, Plan: run.Plan, Context: map[string]string{}}

	store := s.executorCheckpointStore()
	if store == nil {
		// No checkpoint store: replay the whole plan rather than guess a position.
		slog.Warn("ResumeRun: no checkpoint store; restarting the run from step 0", "run", runID)
		return out, nil
	}
	metas, err := store.ListCheckpoints(runID)
	if err != nil {
		return nil, fmt.Errorf("resume: list checkpoints: %w", err)
	}
	if len(metas) == 0 {
		return out, nil // nothing checkpointed yet; start at 0
	}
	// Ascending step order is guaranteed by the zero-padded key, so the last entry really
	// is the furthest this run got.
	latest := metas[len(metas)-1]
	cp, err := store.LoadCheckpoint(runID, latest.StepIndex)
	if err != nil {
		return nil, fmt.Errorf("resume: load checkpoint: %w", err)
	}
	if cp != nil {
		out.Context = cp
	}
	out.StartFrom = latest.StepIndex + 1
	if out.StartFrom > len(run.Plan.Steps) {
		out.StartFrom = len(run.Plan.Steps)
	}
	slog.Info("ResumeRun: continuing", "run", runID, "plan_steps", len(run.Plan.Steps), "start_from", out.StartFrom)
	return out, nil
}

// executorCheckpointStore resolves the checkpoint store.
//
// It prefers the EXPLICIT field. The legacy path below is a runtime type assertion on the
// registry, and it returned nil for the entire life of the feature: the registry holds the
// bbolt adapter in a named field rather than embedding it, so the adapter's checkpoint
// methods were never promoted onto it. The assertion quietly failed, executorCheckpointStore
// returned nil, and the executor's `if d.CheckpointStore != nil` guard skipped every write —
// no error, no test failure, no checkpoints. Explicit wiring is what makes that visible.
func (s *Server) executorCheckpointStore() executer.CheckpointStore {
	if s.Checkpoints != nil {
		return s.Checkpoints
	}
	// Legacy discovery path, kept only so a build that wires a registry-backed store keeps
	// working. Prefer the explicit field: this assertion is what failed silently.
	if s.Manager == nil {
		return nil
	}
	if reg, ok := s.Manager.Registry.(executer.CheckpointStore); ok {
		return reg
	}
	return nil
}
