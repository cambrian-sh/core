package config

import "fmt"

// MemoryNotesConfig is execution.memory_notes: ADR-0139 D14 principal memory notes
// (internal/memorynotes). Every field is read at the composition root
// (app.buildMemoryNotes); zero values take the documented defaults via the methods
// below, so an existing tuning.json needs no new keys.
type MemoryNotesConfig struct {
	// Disabled turns the notes surface off: the agent-plane RPCs answer
	// Unimplemented and the operator handshake does not advertise "memory-notes".
	// Default false (on) — the feature is inert until someone writes a note.
	Disabled bool `json:"disabled,omitempty"`
	// PersonalBudgetChars is the default budget of a personal document, in Unicode
	// code points. 0 ⇒ 4000. An operator budget row (SetMemoryNotesBudget) wins.
	PersonalBudgetChars int `json:"personal_budget_chars,omitempty"`
	// OrgBudgetChars is the default budget of the org document. 0 ⇒ 8000.
	OrgBudgetChars int `json:"org_budget_chars,omitempty"`
	// OrgID names the org scope owner (ADR-0139 D9 org profile). Empty ⇒ no org
	// scope: runs get no org document and agent org writes are SCOPE_DENIED.
	OrgID string `json:"org_id,omitempty"`
	// AgentOrgWrite lets agents write the org document (ProposeMemoryNote scope ORG).
	// Default false: org notes are shared, so editing them is an operator decision.
	AgentOrgWrite bool `json:"agent_org_write,omitempty"`
	// DefaultPrincipal is the run principal used when the lease resolves to none
	// (no conversation owner, no task beneficiary). Meant for the single-principal
	// personal profile. Empty ⇒ such runs get no personal notes (fail-closed).
	DefaultPrincipal string `json:"default_principal,omitempty"`
	// SnapshotTTLSeconds bounds how long a run's frozen snapshot is kept. 0 ⇒ 86400.
	SnapshotTTLSeconds int `json:"snapshot_ttl_seconds,omitempty"`
	// MaxSnapshots caps the frozen-snapshot cache (oldest dropped). 0 ⇒ 4096.
	MaxSnapshots int `json:"max_snapshots,omitempty"`
}

// PersonalBudget is PersonalBudgetChars with its default applied.
func (c MemoryNotesConfig) PersonalBudget() int {
	if c.PersonalBudgetChars > 0 {
		return c.PersonalBudgetChars
	}
	return 4000
}

// OrgBudget is OrgBudgetChars with its default applied.
func (c MemoryNotesConfig) OrgBudget() int {
	if c.OrgBudgetChars > 0 {
		return c.OrgBudgetChars
	}
	return 8000
}

// validate checks execution.memory_notes.
func (c MemoryNotesConfig) validate() []string {
	var errs []string
	if c.PersonalBudgetChars < 0 {
		errs = append(errs, fmt.Sprintf("execution.memory_notes.personal_budget_chars must be >= 0 (got %d)", c.PersonalBudgetChars))
	}
	if c.OrgBudgetChars < 0 {
		errs = append(errs, fmt.Sprintf("execution.memory_notes.org_budget_chars must be >= 0 (got %d)", c.OrgBudgetChars))
	}
	if c.SnapshotTTLSeconds < 0 {
		errs = append(errs, fmt.Sprintf("execution.memory_notes.snapshot_ttl_seconds must be >= 0 (got %d)", c.SnapshotTTLSeconds))
	}
	if c.MaxSnapshots < 0 {
		errs = append(errs, fmt.Sprintf("execution.memory_notes.max_snapshots must be >= 0 (got %d)", c.MaxSnapshots))
	}
	return errs
}
