package config

import (
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ApprovalsConfig is the `approvals` block: the data the OSS approval policy
// reads (ADR-0142 D11; owner acceptance 2026-11-06 item 1). Zero-Hardcode Rule:
// who may answer which action class is configuration, not Go branching.
//
// An absent block is a working deployment: the shipped policy
// (domain.DefaultApprovalPolicyConfig) routes every pre-existing approval kind
// exactly as before — dangerous tools, skill access and proactive plans to an
// operator; a person's own schedules, machine consents, account confirmations
// and publishes to that person (schedules also to an operator).
//
// Consumed by app.buildApprovalHub → domain.NewAddressedHub.
type ApprovalsConfig struct {
	// Rules add or replace policy rows by class. A row whose class matches a
	// shipped row replaces it; a new class is added. Class "*" is the fallback.
	// approvers is any-of: "self", "operator", "person:<id>", "role:<name>",
	// "relationship:<relation>" (resolved by the stage-2 hierarchy; fails
	// closed with state needs_hierarchy until one is installed).
	Rules []ApprovalRuleConfig `json:"rules,omitempty"`
	// NeverSelfApprovable lists action classes no person may approve for
	// themself. Absent ⇒ the shipped list (public_record_release, payment).
	NeverSelfApprovable []string `json:"never_self_approvable,omitempty"`
	// ToolClasses maps a tool name to the action class its approvals carry
	// (e.g. {"stripe_charge": "payment"}), so a policy row can address it.
	ToolClasses map[string]string `json:"tool_classes,omitempty"`
	// StandingMaxTTLHours caps a standing approval's life. 0 ⇒ 720 (30 days);
	// a negative value ⇒ no cap (until revoked).
	StandingMaxTTLHours int `json:"standing_max_ttl_hours,omitempty"`
	// DefaultTimeoutSeconds bounds an approval with no kind-specific window.
	// 0 ⇒ 60 (the historical operator grace window).
	DefaultTimeoutSeconds int `json:"default_timeout_seconds,omitempty"`
	// SweepIntervalSeconds is how often orphaned pending approvals (left by a
	// previous process) are expired and published. 0 ⇒ 30.
	SweepIntervalSeconds int `json:"sweep_interval_seconds,omitempty"`
}

// ApprovalRuleConfig is one policy row.
type ApprovalRuleConfig struct {
	Class         string   `json:"class"`
	Approvers     []string `json:"approvers"`
	AllowStanding bool     `json:"allow_standing,omitempty"`
	// WaitOffline parks a blocking request for its window when no eligible
	// approver is connected, instead of denying it at once (ENG-653). Default
	// false.
	WaitOffline bool `json:"wait_offline,omitempty"`
}

// PolicyConfig merges the block over the shipped policy data.
func (c ApprovalsConfig) PolicyConfig() domain.ApprovalPolicyConfig {
	out := domain.DefaultApprovalPolicyConfig()
	for _, r := range c.Rules {
		row := domain.ApprovalClassRule{Class: r.Class, Approvers: append([]string(nil), r.Approvers...),
			AllowStanding: r.AllowStanding, WaitOffline: r.WaitOffline}
		replaced := false
		for i := range out.Rules {
			if out.Rules[i].Class == r.Class {
				out.Rules[i], replaced = row, true
				break
			}
		}
		if !replaced {
			out.Rules = append(out.Rules, row)
		}
	}
	if c.NeverSelfApprovable != nil {
		out.NeverSelfApprovable = append([]string(nil), c.NeverSelfApprovable...)
	}
	if len(c.ToolClasses) > 0 {
		out.ToolClasses = map[string]string{}
		for k, v := range c.ToolClasses {
			out.ToolClasses[k] = v
		}
	}
	switch {
	case c.StandingMaxTTLHours < 0:
		out.StandingMaxTTL = 0
	case c.StandingMaxTTLHours > 0:
		out.StandingMaxTTL = time.Duration(c.StandingMaxTTLHours) * time.Hour
	}
	return out
}

// DefaultTimeout is the resolved default approval window.
func (c ApprovalsConfig) DefaultTimeout() time.Duration {
	if c.DefaultTimeoutSeconds > 0 {
		return time.Duration(c.DefaultTimeoutSeconds) * time.Second
	}
	return 60 * time.Second
}

// SweepInterval is the resolved orphan-sweep interval.
func (c ApprovalsConfig) SweepInterval() time.Duration {
	if c.SweepIntervalSeconds > 0 {
		return time.Duration(c.SweepIntervalSeconds) * time.Second
	}
	return 30 * time.Second
}

// Validate builds the policy once, so a bad rule fails the config load rather
// than the first approval.
func (c ApprovalsConfig) Validate() *ConfigError {
	if _, err := domain.NewConfigApprovalPolicy(c.PolicyConfig()); err != nil {
		return &ConfigError{Field: "approvals", Message: err.Error()}
	}
	return nil
}
