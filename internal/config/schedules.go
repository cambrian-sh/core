package config

import (
	"fmt"
	"time"
)

// SchedulesConfig is execution.schedules: principal-owned recurring tasks an
// assistant proposes for the person it serves
// (T-20261106-the-assistant-cannot-set-up-a-recurring-task). Read at the
// composition root (app: the agent-plane scheduled-task service); zero values take
// the defaults below, so an existing tuning.json needs no new keys.
type SchedulesConfig struct {
	// Disabled turns the agent-plane scheduled-task RPCs off (Unimplemented) and
	// withholds the "scheduled-tasks" capability. Default false (on) — the surface
	// is inert without a premium schedule store anyway.
	Disabled bool `json:"disabled,omitempty"`
	// MaxPerPrincipal caps how many scheduled tasks one principal may hold.
	// 0 ⇒ 20. A negative value is a config error.
	MaxPerPrincipal int `json:"max_per_principal,omitempty"`
	// MinIntervalSeconds is the shortest allowed gap between two firings of one
	// task. 0 ⇒ 3600 (hourly). Set it lower (e.g. 60) on a test deployment.
	MinIntervalSeconds int `json:"min_interval_seconds,omitempty"`
	// ApprovalTimeoutSeconds is how long a proposal waits for the principal's
	// approval (the approval hub / console Inbox) before it is dropped with
	// NO_DECISION — nothing is created without an explicit approve. 0 ⇒ 300.
	ApprovalTimeoutSeconds int `json:"approval_timeout_seconds,omitempty"`
}

// MaxPerPrincipalOrDefault is MaxPerPrincipal with its default applied.
func (c SchedulesConfig) MaxPerPrincipalOrDefault() int {
	if c.MaxPerPrincipal > 0 {
		return c.MaxPerPrincipal
	}
	return 20
}

// MinInterval is MinIntervalSeconds with its default applied.
func (c SchedulesConfig) MinInterval() time.Duration {
	if c.MinIntervalSeconds > 0 {
		return time.Duration(c.MinIntervalSeconds) * time.Second
	}
	return time.Hour
}

// ApprovalTimeout is ApprovalTimeoutSeconds with its default applied.
func (c SchedulesConfig) ApprovalTimeout() time.Duration {
	if c.ApprovalTimeoutSeconds > 0 {
		return time.Duration(c.ApprovalTimeoutSeconds) * time.Second
	}
	return 5 * time.Minute
}

// validate checks execution.schedules.
func (c SchedulesConfig) validate() []string {
	var errs []string
	if c.MaxPerPrincipal < 0 {
		errs = append(errs, fmt.Sprintf("execution.schedules.max_per_principal must be >= 0 (got %d)", c.MaxPerPrincipal))
	}
	if c.MinIntervalSeconds < 0 {
		errs = append(errs, fmt.Sprintf("execution.schedules.min_interval_seconds must be >= 0 (got %d)", c.MinIntervalSeconds))
	}
	if c.ApprovalTimeoutSeconds < 0 {
		errs = append(errs, fmt.Sprintf("execution.schedules.approval_timeout_seconds must be >= 0 (got %d)", c.ApprovalTimeoutSeconds))
	}
	return errs
}
