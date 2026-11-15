package app

import (
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// executorLeases are the per-lease controls of the tool reference monitor,
// built from config BEFORE the executor so they are constructor controls
// (AGENTS.md §2.3; T-20261113-domain-toolexecutor-has-no-constructor-its).
// They used to be chained into ToolExecutor.LeaseCeiling by wireComposedWorkers
// and wireSubtasks after construction, so whether a composed or subtask lease
// was held to its toolset depended on those two calls being reached, in order.
//
// Each source is empty until its wiring binds leases into it (the composer
// binds compositions, the subtask tool registers parents and children), so a
// source built here whose consumer later turns out not to be wired (e.g. the
// dispatcher is missing) bounds nothing — an unbound lease is unaffected, as
// before.
type executorLeases struct {
	// composed: the ADR-0145 D12 compositions; nil when execution.workers.composed is off.
	composed *domain.ComposedLeases
	// subtasks: the ADR-0150 subtask registry (D12 ceiling, effect refusal,
	// deterministic write keys); nil when run_subtasks is not served.
	subtasks *domain.Subtasks
}

// buildExecutorLeases reads the same keys wireComposedWorkers and wireSubtasks
// read, so a source exists exactly when its wiring can bind into it.
// actions is the ADR-0154 durable action record (nil: memory only).
func buildExecutorLeases(cfg config.ExecutionConfig, actions domain.ActionRecordStore) executorLeases {
	var l executorLeases
	if !cfg.Workers.Composed {
		return l
	}
	l.composed = domain.NewComposedLeases()
	if cfg.Lane.Subtasks.On() && cfg.Lane.IsSingleAgent() {
		l.subtasks = domain.NewSubtasks(idempotencyTTL, actions)
	}
	return l
}

// controls puts the lease sources into the executor's constructor controls.
func (l executorLeases) controls(c domain.ToolExecutorControls) domain.ToolExecutorControls {
	var ceilings []domain.LeaseToolCeiling
	if l.composed != nil {
		ceilings = append(ceilings, l.composed)
	}
	if l.subtasks != nil {
		ceilings = append(ceilings, l.subtasks)
		c.Subtasks = l.subtasks
	}
	c.LeaseCeiling = domain.ChainLeaseCeilings(ceilings...)
	return c
}
