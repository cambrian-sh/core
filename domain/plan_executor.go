package domain

import "context"

// PlanExecutor runs an already-generated ExecutionPlan through the kernel's real DAG
// executor — the same path an Orchestrator.Execute plan takes: topological validation,
// per-step auction dispatch, per-step ADR-0018 budget leases, session + run persistence,
// checkpoints, telemetry. It is the port a plugin uses to EXECUTE a plan; generating one
// is ReactivePlanner's job.
//
// It exists so the premium `start_plan` watch/pipeline action (PREM-0032) can run the
// plan it generated instead of handing it to a no-op. The kernel's DAGExecutor is a
// per-execution object assembled inline from a dozen server-owned collaborators, so the
// port names the OUTCOME ("run this plan") rather than exposing that struct: a plugin
// that assembled its own executor would be a second execution pathway (AGENTS.md §4.3).
//
// h carries the originating agent and any context seeded into the plan's initial
// context map; it may be nil. The returned Handoff's Context holds the plan's master
// context (with `_session_id` when the run was attached to a session) and its Payload
// the final step's result.
type PlanExecutor interface {
	Execute(ctx context.Context, plan *ExecutionPlan, h *Handoff) (*Handoff, error)
}
