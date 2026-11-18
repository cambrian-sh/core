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
	// Composed turns on composed dispatch. Default true (owner 2026-11-13, ADR-0150:
	// the single-agent lane's default agent and every run_subtasks worker are the
	// composed runtime, so without it the default lane has no tools —
	// T-20261113-default-single-agent-lane-has-no-tools-out-of). On: a plan step whose
	// eligible executors (the gatekeeper's capability and effect check) include
	// declarative profile agents is run by ONE runtime agent (agents/worker_agent.py
	// --composed) that the kernel composes for the step: the professions' skills,
	// the tools inside the professions' allowlists AND the act-for person's scope
	// (ADR-0145 D14: a step's declared effects do not narrow them), the brief and the step's ledger references.
	// The composition is bound to the step's lease (the model can narrow it, never
	// widen it) and recorded in the run ledger. A step whose selected executor is a
	// code or service agent (no profile) is dispatched to that agent as before.
	// Off: the winner's pre-built agent runs the step (the legacy path). Requires
	// execution.ledger.enabled. No omitempty: an explicit false must survive a
	// marshal of the effective config, or the true default would come back.
	Composed bool `json:"composed"`
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
	// MaxToolRounds is the composed runtime's tool-call budget per step (the SDK
	// ReAct loop's max_tool_rounds), handed to it as CAMBRIAN_COMPOSED_MAX_TOOL_ROUNDS.
	// It was the SDK's built-in 12, which cut a progressing sandbox analysis
	// ("Tool budget exhausted after 12 calls", personal run 2026-11-09 p06#3,
	// T-20261109-per-step-deadline-step-timeout-floor-ms-180-s). Under
	// decisions/no-turn-budgets-for-now-terminate-on-progress.md run configurations
	// set it very high; it stays as a runaway backstop. 0 ⇒ the runtime's default (12).
	MaxToolRounds int `json:"max_tool_rounds,omitempty"`
}

// RuntimeID is RuntimeAgentID with its default applied.
func (c WorkersConfig) RuntimeID() string {
	if c.RuntimeAgentID != "" {
		return c.RuntimeAgentID
	}
	return DefaultComposedRuntimeAgentID
}

// ThirdPartyRuntimeSuffix names the third-party composed runtime: RuntimeID()
// plus this suffix (ThirdPartyRuntimeID).
const ThirdPartyRuntimeSuffix = "_third_party"

// ThirdPartyRuntimeID is the id of the SECOND composed runtime, the one launched
// at the ADR-0139 D8 third-party tier (kernel-only network, low privilege, the
// third-party memory cap; ADR-0157 D3). A composition holding a third-party
// profession runs on it, never on the shared runtime (owner decision
// 2026-11-18, ENG-2083). Derived from RuntimeID, so it needs no key of its own.
func (c WorkersConfig) ThirdPartyRuntimeID() string {
	return c.RuntimeID() + ThirdPartyRuntimeSuffix
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
	if c.MaxToolRounds < 0 {
		errs = append(errs, fmt.Sprintf("execution.workers.max_tool_rounds must be >= 0 (got %d; 0 = the runtime default)", c.MaxToolRounds))
	}
	if c.RuntimeAgentID != "" && !agentIDPattern.MatchString(c.RuntimeAgentID) {
		errs = append(errs, fmt.Sprintf("execution.workers.runtime_agent_id %q must match %s", c.RuntimeAgentID, agentIDPattern))
	} else if c.Composed && !agentIDPattern.MatchString(c.ThirdPartyRuntimeID()) {
		errs = append(errs, fmt.Sprintf("execution.workers.runtime_agent_id %q is too long: the third-party runtime %q must match %s",
			c.RuntimeAgentID, c.ThirdPartyRuntimeID(), agentIDPattern))
	}
	if c.Composed && !ledgerOn {
		errs = append(errs, "execution.workers.composed requires execution.ledger.enabled: every composition is recorded in the run ledger")
	}
	return errs
}
