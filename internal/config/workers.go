package config

import (
	"fmt"
	"regexp"
)

// DefaultComposedRuntimeAgentID is the agent id the composed-worker runtime is
// registered under when execution.workers.composed is on.
const DefaultComposedRuntimeAgentID = "composed_worker"

// DefaultComposedMaxSkills caps the skill index of one composed worker.
const DefaultComposedMaxSkills = 8

// WorkersConfig is execution.workers: ADR-0145 D12 (workers composed by the kernel)
// and the D3 evidence re-prompt of the SDK's ReAct runtime. Every field is read at
// the composition root (app/composed_workers.go, internal/kernel/metabolism_stack.go);
// zero values take the documented defaults, so an existing tuning.json needs no keys.
type WorkersConfig struct {
	// Composed turns on composed dispatch. Default false. On: a plan step whose
	// eligible executors (the gatekeeper's capability and effect check) include
	// declarative profile agents is run by ONE runtime agent (agents/worker_agent.py
	// --composed) that the kernel composes for the step: the professions' skills,
	// the tools inside the professions' allowlists AND the act-for person's scope
	// (ADR-0145 D14: a step's declared effects do not narrow them), the brief and the step's ledger references.
	// The composition is bound to the step's lease (the model can narrow it, never
	// widen it) and recorded in the run ledger. A step whose selected executor is a
	// code or service agent (no profile) is dispatched to that agent as before.
	// Off: the winner's pre-built agent runs the step (the legacy path). Requires
	// execution.ledger.enabled.
	Composed bool `json:"composed,omitempty"`
	// RuntimeAgentID is the id the composed runtime is registered and dispatched
	// under. "" ⇒ DefaultComposedRuntimeAgentID.
	RuntimeAgentID string `json:"runtime_agent_id,omitempty"`
	// MaxSkills caps the composed skill index (sorted by name, the first MaxSkills
	// kept). 0 ⇒ DefaultComposedMaxSkills.
	MaxSkills int `json:"max_skills,omitempty"`
	// EvidenceReprompt turns on the ADR-0145 D3 re-prompt fix in every SDK agent
	// the kernel spawns (environment CAMBRIAN_EVIDENCE_REPROMPT=1): a plain-text
	// reply in a run that gathered no evidence (no tool ran, no memory hit, nothing
	// seeded) is re-prompted toward delegation or the next tool, not toward
	// final_answer. Default false.
	EvidenceReprompt bool `json:"evidence_reprompt,omitempty"`
}

// RuntimeID is RuntimeAgentID with its default applied.
func (c WorkersConfig) RuntimeID() string {
	if c.RuntimeAgentID != "" {
		return c.RuntimeAgentID
	}
	return DefaultComposedRuntimeAgentID
}

// SkillCap is MaxSkills with its default applied.
func (c WorkersConfig) SkillCap() int {
	if c.MaxSkills > 0 {
		return c.MaxSkills
	}
	return DefaultComposedMaxSkills
}

// agentIDPattern is the profile-name rule (internal/agentprofile), which the
// runtime id must also satisfy: it is registered beside the profile agents.
var agentIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

// validate checks execution.workers. ledgerOn is execution.ledger.enabled.
func (c WorkersConfig) validate(ledgerOn bool) []string {
	var errs []string
	if c.MaxSkills < 0 {
		errs = append(errs, fmt.Sprintf("execution.workers.max_skills must be >= 0 (got %d)", c.MaxSkills))
	}
	if c.RuntimeAgentID != "" && !agentIDPattern.MatchString(c.RuntimeAgentID) {
		errs = append(errs, fmt.Sprintf("execution.workers.runtime_agent_id %q must match %s", c.RuntimeAgentID, agentIDPattern))
	}
	if c.Composed && !ledgerOn {
		errs = append(errs, "execution.workers.composed requires execution.ledger.enabled: every composition is recorded in the run ledger")
	}
	return errs
}
