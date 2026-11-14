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

// evidenceToolControls reads the executor-side ADR-0145 D9/D14 flags into the
// security controls the tool executor is CONSTRUCTED with (domain.NewToolExecutor,
// AGENTS.md §2.3; T-20261113-domain-toolexecutor-has-no-constructor-its): before,
// wireEvidencePipeline assigned them onto the built executor afterwards.
//
//   - execution.tools.write_targets → WriteTargets (always: the plan action ledger
//     records each write's target, which coverage matches write items against,
//     T-20261112-coverage-settles-a-write-item-on-any-done)
//   - execution.tools.request_guards → Guards (same write-target rule set)
//   - execution.tools.validate_args → ArgValidator (internal/toolargs)
//   - execution.tools.idempotent_writes → Idempotency (durable via the ADR-0154
//     action record when actions is non-nil)
//
// It fills only those fields; the caller supplies the required controls.
func evidenceToolControls(cfg config.ExecutionConfig, actions domain.ActionRecordStore, c domain.ToolExecutorControls) domain.ToolExecutorControls {
	c.WriteTargets = cfg.Tools.WriteTargets.Domain()
	if cfg.Tools.RequestGuards {
		c.Guards = &domain.RequestGuards{
			Targets:  c.WriteTargets,
			Writes:   domain.NewWriteRecord(idempotencyTTL),
			Refusals: domain.NewRefusalRecord(0),
		}
	}
	if cfg.Tools.ValidateArgs {
		c.ArgValidator = toolargs.Validator{}
	}
	if cfg.Tools.IdempotentWrites {
		c.Idempotency = domain.NewDurableIdempotencyCache(idempotencyTTL, actions)
	}
	return c
}

// wireEvidencePipeline threads the ADR-0145 phase-2 flags to their consumers in
// one place (T-20261108-adr-0145-phase-2-requirement-checklist-with), each read
// here and nowhere else (AGENTS.md §3.4):
//
//   - execution.tools.request_guards / write_targets / validate_args /
//     idempotent_writes → the executor's constructor controls (evidenceToolControls
//     above, read where the executor is built; ADR-0145 D9/D14)
//   - execution.tools.constraint_classes → the checklist deriver's constraints
//   - execution.plan.checklist_enabled → Server.Checklist (requirements.Deriver over
//     the planner's generator); the server also reads the flag (checklistOn), and it
//     needs the run ledger (config validation refuses it without one)
//   - execution.plan.plan_validator (+ _max_repairs, coverage_max_stages) are read by
//     the server from ExecCfg directly.
//
// gen is the planner organ's generator (it renders request-shaped prompts).
//
// actions is the ADR-0154 durable action record (nil without a database): when
// set, idempotent writes are also recorded there and survive a restart.
func wireEvidencePipeline(cfg config.ExecutionConfig, server *subnetwork.Server, gen domain.Generator, actions domain.ActionRecordStore) {
	if server == nil || server.ToolExecutor == nil {
		return
	}
	terms, err := cfg.Tools.ConstraintTerms()
	if err != nil {
		slog.Error("ADR-0145 D14: execution.tools.constraint_classes invalid; checklist constraints are OFF", "err", err)
		terms = nil
	}
	te := server.ToolExecutor
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
		"request_guards", cfg.Tools.RequestGuards, "validate_args", cfg.Tools.ValidateArgs, "idempotent_writes", cfg.Tools.IdempotentWrites,
		"durable_action_record", actions != nil)
}
