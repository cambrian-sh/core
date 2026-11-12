package app

import (
	"log/slog"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/requirements"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
	"github.com/cambrian-sh/core/internal/toolargs"
)

// idempotencyTTL is how long a side-effecting call's stored result answers a
// retry (ADR-0145 D9). A plan's run does not outlive a day; a restart forgets it.
const idempotencyTTL = 24 * time.Hour

// wireEvidencePipeline threads the ADR-0145 phase-2 flags to their consumers in
// one place (T-20261108-adr-0145-phase-2-requirement-checklist-with), each read
// here and nowhere else (AGENTS.md §3.4):
//
//   - execution.tools.request_guards (+ write_targets, constraint_classes) →
//     ToolExecutor.Guards (ADR-0145 D14; the D9 effect menu it replaced was removed)
//   - execution.tools.validate_args  → ToolExecutor.ArgValidator (internal/toolargs)
//   - execution.tools.idempotent_writes → ToolExecutor.Idempotency
//   - execution.plan.checklist_enabled → Server.Checklist (requirements.Deriver over
//     the planner's generator); the server also reads the flag (checklistOn), and it
//     needs the run ledger (config validation refuses it without one)
//   - execution.plan.plan_validator (+ _max_repairs, coverage_max_stages) are read by
//     the server from ExecCfg directly.
//
// gen is the planner organ's generator (it renders request-shaped prompts).
func wireEvidencePipeline(cfg config.ExecutionConfig, server *subnetwork.Server, gen domain.Generator) {
	if server == nil || server.ToolExecutor == nil {
		return
	}
	terms, err := cfg.Tools.ConstraintTerms()
	if err != nil {
		slog.Error("ADR-0145 D14: execution.tools.constraint_classes invalid; checklist constraints are OFF", "err", err)
		terms = nil
	}
	te := server.ToolExecutor
	// The write-target rules identify every side-effecting call's target in the
	// plan action ledger, which coverage matches write items against
	// (T-20261112-coverage-settles-a-write-item-on-any-done); the request guards,
	// when on, use the same rule set.
	te.WriteTargets = cfg.Tools.WriteTargets.Domain()
	if cfg.Tools.RequestGuards {
		te.Guards = &domain.RequestGuards{
			Targets:  te.WriteTargets,
			Writes:   domain.NewWriteRecord(idempotencyTTL),
			Refusals: domain.NewRefusalRecord(0),
		}
	}
	if cfg.Tools.ValidateArgs {
		te.ArgValidator = toolargs.Validator{}
	}
	if cfg.Tools.IdempotentWrites {
		te.Idempotency = domain.NewIdempotencyCache(idempotencyTTL)
	}
	if cfg.Plan.ChecklistEnabled {
		if gen == nil {
			slog.Error("ADR-0145 D5: execution.plan.checklist_enabled is set but there is no planner generator; the checklist is OFF")
			return
		}
		server.Checklist = &requirements.Deriver{LLM: gen, Constraints: terms}
		// Workers cite the ledger entry of every tool result in their envelopes.
		te.ReturnLedgerRef = true
		if server.Ledger == nil {
			slog.Error("ADR-0145 D5: execution.plan.checklist_enabled is set but the run ledger is not wired; the checklist is OFF")
		}
	}
	slog.Info("ADR-0145 phase 2", "checklist", cfg.Plan.ChecklistEnabled, "plan_validator", cfg.Plan.PlanValidator,
		"request_guards", cfg.Tools.RequestGuards, "validate_args", cfg.Tools.ValidateArgs, "idempotent_writes", cfg.Tools.IdempotentWrites)
}
