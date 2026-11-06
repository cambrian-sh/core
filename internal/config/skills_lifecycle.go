package config

import "strings"

// SkillsConfig is execution.skills: the ADR-0139 D13 skill lifecycle (agent
// proposals, the approval queue, and the deterministic curator). Consumed by
// app.go (wireSkillLifecycle) → internal/skill/lifecycle.
type SkillsConfig struct {
	// ProposalsEnabled lets agents call Orchestrator/ProposeSkill. A proposal is
	// inert until an operator approves it, so this defaults on; off makes every
	// proposal DENIED. Needs the Postgres lifecycle store (migration 0029).
	ProposalsEnabled bool `json:"proposals_enabled"`
	// MaxPendingPerAgent caps one agent's PENDING proposals (QUOTA beyond it).
	MaxPendingPerAgent int `json:"max_pending_per_agent"`
	// MaxSkillMDChars caps a proposed SKILL.md (INVALID_FORMAT beyond it).
	MaxSkillMDChars int `json:"max_skill_md_chars"`
	// Curator is the deterministic (no-LLM) active → stale → archived lifecycle.
	Curator SkillCuratorConfig `json:"curator"`
	// AccessRequestTimeoutSeconds is how long a profile agent's use_skill on a skill
	// outside its effective set (profile skills + grants) waits for an operator on
	// the approval hub before it answers "pending" (decisions/skill-access-grants.md).
	// The request stays open; an approval after that grants it for later runs.
	// 0 = the default 120. Consumed by app.go → internal/skill/access.
	AccessRequestTimeoutSeconds int `json:"access_request_timeout_seconds"`
	// AccessAutoApprove grants every skill access request without asking. Separate
	// from tools.tools_auto_approve on purpose: that flag is about dangerous tools,
	// this one widens what agents may read. Default off.
	AccessAutoApprove bool `json:"access_auto_approve"`
}

// SkillCuratorConfig is execution.skills.curator. Off by default.
type SkillCuratorConfig struct {
	// Enabled starts the background curator worker.
	Enabled bool `json:"enabled"`
	// StaleAfterDays: an active skill unused this long becomes stale (still served).
	StaleAfterDays int `json:"stale_after_days"`
	// ArchiveAfterDays: a stale skill unused this long (from its last use or
	// activation) becomes archived (no longer served; never deleted). Must exceed
	// stale_after_days.
	ArchiveAfterDays int `json:"archive_after_days"`
	// IntervalMinutes between curator passes.
	IntervalMinutes int `json:"interval_minutes"`
	// Sources the curator manages. Default ["agent-proposed"]: shipped builtin
	// skills are repository content an operator chose to deploy.
	Sources []string `json:"sources"`
	// Pinned skills are exempt from the curator.
	Pinned []string `json:"pinned"`
}

// DefaultSkillsConfig is the shipped default: proposals on (inert until
// approved), curator off.
func DefaultSkillsConfig() SkillsConfig {
	return SkillsConfig{
		ProposalsEnabled:   true,
		MaxPendingPerAgent: 20,
		MaxSkillMDChars:    32000,
		// decisions/skill-access-grants.md: wait two minutes for an operator.
		AccessRequestTimeoutSeconds: 120,
		Curator: SkillCuratorConfig{
			StaleAfterDays:   30,
			ArchiveAfterDays: 90,
			IntervalMinutes:  60,
			Sources:          []string{"agent-proposed"},
			Pinned:           []string{},
		},
	}
}

func (s SkillsConfig) validate() []string {
	var errs []string
	if s.MaxPendingPerAgent < 0 {
		errs = append(errs, "skills.max_pending_per_agent must be >= 0 (0 = the default 20)")
	}
	if s.MaxSkillMDChars < 0 {
		errs = append(errs, "skills.max_skill_md_chars must be >= 0 (0 = the default 32000)")
	}
	if s.AccessRequestTimeoutSeconds < 0 {
		errs = append(errs, "skills.access_request_timeout_seconds must be >= 0 (0 = the default 120)")
	}
	c := s.Curator
	if c.Enabled {
		if c.StaleAfterDays <= 0 {
			errs = append(errs, "skills.curator.stale_after_days must be > 0 when the curator is enabled")
		}
		if c.ArchiveAfterDays <= c.StaleAfterDays {
			errs = append(errs, "skills.curator.archive_after_days must exceed stale_after_days")
		}
		if c.IntervalMinutes <= 0 {
			errs = append(errs, "skills.curator.interval_minutes must be > 0 when the curator is enabled")
		}
	}
	for _, src := range c.Sources {
		switch strings.TrimSpace(src) {
		case "builtin", "official", "hub", "agent-proposed":
		default:
			errs = append(errs, "skills.curator.sources: unknown source "+src+" (want builtin | official | hub | agent-proposed)")
		}
	}
	return errs
}
