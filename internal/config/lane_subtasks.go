package config

import (
	"fmt"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0150 D2/D6/D10: execution.lane.subtasks, the kernel tool run_subtasks
// (internal/subtasks). Zero values take the documented defaults (the methods
// below), so an existing tuning.json needs no keys. Every field is read where
// the tool is wired (app/subtasks.go) — AGENTS.md §3.4.

// Subtask defaults (ADR-0150 D10; the cap values are the ADR's proposals,
// owner question 1).
const (
	DefaultSubtaskMaxParallel         = 4
	DefaultSubtaskMaxPerCall          = 8
	DefaultSubtaskMaxPerRequest       = 64
	DefaultSubtaskMaxDepth            = 1
	DefaultSubtaskSummaryMaxChars     = 4000
	DefaultSubtaskWorkerMaxToolRounds = 200
	DefaultSubtaskWorkerMaxWall       = 900 * time.Second
)

// DefaultSubtaskAllowedEffects is the effect ceiling of a subtask's lease: read
// and read-the-web, never send (owner Q3, 2026-11-13; ADR-0150 D2/D9).
var DefaultSubtaskAllowedEffects = []string{string(domain.EffectRead), string(domain.EffectEgressRead)}

// LaneSubtasksConfig is execution.lane.subtasks (ADR-0150 D10).
type LaneSubtasksConfig struct {
	// Enabled offers run_subtasks to the single-agent lane's agent (nil ⇒ true).
	// Off: the tool is not registered at all.
	Enabled *bool `json:"enabled,omitempty"`
	// MaxParallel is how many subtasks of one call run at once; the rest queue in
	// order. 0 ⇒ 4 (Anthropic's 3-5).
	MaxParallel int `json:"max_parallel,omitempty"`
	// MaxPerCall refuses (never truncates) a call with more subtasks. 0 ⇒ 8.
	MaxPerCall int `json:"max_per_call,omitempty"`
	// MaxPerRequest caps the subtasks one parent run spawns (D4.5). 0 ⇒ 64.
	MaxPerRequest int `json:"max_per_request,omitempty"`
	// MaxDepth is how deep subtasks may nest (D6): 1 ⇒ a subtask cannot spawn
	// subtasks (refused at the kernel). 0 ⇒ 1.
	MaxDepth int `json:"max_depth,omitempty"`
	// AllowedEffects is the subtask lease's effect ceiling (ADR-0086 classes),
	// enforced by the ToolExecutor whatever the authorizer is. Empty ⇒
	// ["read","egress_read"]. Plain "egress" means both egress_read and egress_send.
	AllowedEffects []string `json:"allowed_effects,omitempty"`
	// SummaryMaxChars bounds each subtask's summary; the full answer is a ledger
	// entry (result_ref). 0 ⇒ 4000.
	SummaryMaxChars int `json:"summary_max_chars,omitempty"`
	// WorkerMaxToolRounds is a subtask worker's runaway cap on tool rounds (D4).
	// 0 ⇒ 200.
	WorkerMaxToolRounds int `json:"worker_max_tool_rounds,omitempty"`
	// WorkerMaxWallSeconds is a subtask worker's runaway cap on wall time (D4).
	// 0 ⇒ 900.
	WorkerMaxWallSeconds int `json:"worker_max_wall_seconds,omitempty"`
}

// On reports whether run_subtasks is enabled (default true).
func (c LaneSubtasksConfig) On() bool { return c.Enabled == nil || *c.Enabled }

// Parallel is MaxParallel with its default.
func (c LaneSubtasksConfig) Parallel() int {
	return orDefault(c.MaxParallel, DefaultSubtaskMaxParallel)
}

// PerCall is MaxPerCall with its default.
func (c LaneSubtasksConfig) PerCall() int { return orDefault(c.MaxPerCall, DefaultSubtaskMaxPerCall) }

// PerRequest is MaxPerRequest with its default.
func (c LaneSubtasksConfig) PerRequest() int {
	return orDefault(c.MaxPerRequest, DefaultSubtaskMaxPerRequest)
}

// Depth is MaxDepth with its default.
func (c LaneSubtasksConfig) Depth() int { return orDefault(c.MaxDepth, DefaultSubtaskMaxDepth) }

// SummaryChars is SummaryMaxChars with its default.
func (c LaneSubtasksConfig) SummaryChars() int {
	return orDefault(c.SummaryMaxChars, DefaultSubtaskSummaryMaxChars)
}

// WorkerToolRounds is WorkerMaxToolRounds with its default.
func (c LaneSubtasksConfig) WorkerToolRounds() int {
	return orDefault(c.WorkerMaxToolRounds, DefaultSubtaskWorkerMaxToolRounds)
}

// WorkerWall is WorkerMaxWallSeconds with its default.
func (c LaneSubtasksConfig) WorkerWall() time.Duration {
	if c.WorkerMaxWallSeconds > 0 {
		return time.Duration(c.WorkerMaxWallSeconds) * time.Second
	}
	return DefaultSubtaskWorkerMaxWall
}

// Effects is AllowedEffects as domain effects, with its default. Validation has
// already refused a value outside the closed set.
func (c LaneSubtasksConfig) Effects() []domain.ToolEffect {
	names := c.AllowedEffects
	if len(names) == 0 {
		names = DefaultSubtaskAllowedEffects
	}
	out := make([]domain.ToolEffect, 0, len(names))
	for _, n := range names {
		out = append(out, domain.ToolEffect(n))
	}
	return out
}

func (c LaneSubtasksConfig) validate() []string {
	var errs []string
	for k, v := range map[string]int{
		"max_parallel": c.MaxParallel, "max_per_call": c.MaxPerCall, "max_per_request": c.MaxPerRequest,
		"max_depth": c.MaxDepth, "summary_max_chars": c.SummaryMaxChars,
		"worker_max_tool_rounds": c.WorkerMaxToolRounds, "worker_max_wall_seconds": c.WorkerMaxWallSeconds,
	} {
		if v < 0 {
			errs = append(errs, fmt.Sprintf("execution.lane.subtasks.%s must be >= 0 (0 = default)", k))
		}
	}
	for _, e := range c.AllowedEffects {
		if !domain.ValidToolEffect(domain.ToolEffect(e)) {
			errs = append(errs, fmt.Sprintf("execution.lane.subtasks.allowed_effects: %q is outside the closed effect set", e))
		}
	}
	if c.MaxPerCall > 0 && c.MaxPerRequest > 0 && c.MaxPerCall > c.MaxPerRequest {
		errs = append(errs, "execution.lane.subtasks.max_per_call must not exceed max_per_request")
	}
	return errs
}
