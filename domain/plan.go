package domain

// Agent-pin strengths for Step.AgentPin / DispatchTask.AgentPin.
//
// The pin exists because the planner was ALREADY trying to name agents and had no
// field to name them in: its prompt invites "you may reference a specific agent ID",
// so it smuggled `terminal_agent` into required_capabilities, where a working L1 gate
// turned it into a requirement nothing could satisfy and the step died with no
// candidates (measured 2026-07-28). Naming an agent is a legitimate user instruction —
// it needs a channel of its own, separate from the capability contract.
//
// This is not a Zero-Hardcode breach: the agent name arrives as DATA on a plan step,
// derived from what the user asked for. The rule forbids authored agent-routing tables
// in Go, not honouring an explicit human directive.
const (
	// PinSoft prioritises the named agent without guaranteeing it: exempt from the L2
	// semantic gate and given a merit boost, but still subject to the L1 capability
	// contract and still able to lose to a stronger candidate. An unknown name
	// degrades to ordinary selection — a soft pin never strands a step.
	PinSoft = "soft"
	// PinHard binds the named agent directly, skipping capability discovery and the
	// semantic gate — but NOT the effect check: a hard-pinned agent whose tools cannot
	// perform the step's required effects is refused (gatekeeper.FindCandidates).
	// Reserved for an imperative user instruction ("use X to do Y"), where silently
	// routing elsewhere would be a worse answer than failing. An unavailable name is
	// an error, not a fallback.
	PinHard = "hard"
)

type Step struct {
	Query           string  `json:"query"`
	DependsOn       []int   `json:"depends_on,omitempty"`
	IsThought       bool    `json:"is_thought,omitempty"`
	MaxEnergy       float64 `json:"max_energy,omitempty"`
	CheckpointAfter bool    `json:"checkpoint_after,omitempty"`
	// CheckpointQuery (json "checkpoint_query", ADR-0013) was deleted 2026-11-14
	// (owner decision, ENG-1313): its only reader, the checkpoint gate, went with
	// ADR-0139 D10. Stored plans that still carry the key decode with it ignored.
	CacheTTLSeconds int `json:"cache_ttl_seconds,omitempty"`
	// RequiredCapabilities is the ROUTE-03 capability contract: the capability
	// tags a step needs its executor to declare, emitted by the planner from the
	// live capability-cluster vocabulary. When non-empty AND the capability
	// contract is enabled, L1 Declaration hard-gates candidates on
	// required ⊆ manifest.Capabilities. Empty ⇒ today's behavior (backward
	// compatible). Populated only under the capability_contract arm.
	RequiredCapabilities []string `json:"required_capabilities,omitempty"`

	// RequiredEffects are the effects an ACTION step needs its executor's tools to
	// produce, from the configured vocabulary (execution.capability.effect_vocabulary:
	// read, write, move, delete, execute, egress, send, spend, admin). Dispatch admits
	// only agents whose tool-derived effects cover every one, and refuses the step
	// when none does (domain/step_effects.go). nil ⇒ the planner declared nothing
	// (a legacy plan): the step dispatches as before, logged "unverified". An empty
	// non-nil list is the planner saying the step needs no effect.
	RequiredEffects []string `json:"required_effects,omitempty"`

	// PreferredAgent names the agent this step should run on, emitted by the planner
	// when the USER named one. Empty ⇒ ordinary discovery + selection, which stays the
	// default: the planner is instructed to describe capabilities, not pick agents.
	PreferredAgent string `json:"preferred_agent,omitempty"`

	// AgentPin is the strength of PreferredAgent — PinSoft or PinHard. Empty with a
	// non-empty PreferredAgent reads as PinSoft, so an unspecified or malformed pin
	// degrades to the weaker, non-stranding behaviour.
	AgentPin string `json:"agent_pin,omitempty"`

	// FanOutOver (ADR-0051 D10 / ADR-0078 R2) makes this a PARAMETRIC step: it names
	// the index of a prior step whose output supplies a SET, and at runtime the
	// executor expands this one step into N concrete children — one per item — by
	// deterministic template substitution of FanOutVar. It is how a plan adapts to a
	// cardinality only discovery can reveal ("scan the folder → write the N missing
	// sections") WITHOUT a planner round-trip. nil ⇒ an ordinary step.
	//
	// Expansion is deterministic given the source output, which is why it is a
	// sanctioned in-execution-editing exception rather than a breach of DAG freeze:
	// the executor rewrites the plan, never the (untrusted) executing agent.
	FanOutOver *int `json:"fan_out_over,omitempty"`

	// FanOutVar is the template variable substituted per item in Query. "" ⇒ "item",
	// i.e. Query "write the file for {item}" becomes one step per discovered item.
	FanOutVar string `json:"fan_out_var,omitempty"`

	// FanOutItem is set only on a CONCRETE CHILD of a fan-out expansion (ExpandFanOut):
	// the one item this child was made for. Non-empty ⇒ this step is a fan-out child, so
	// the executor runs it under the execution.plan.fan_out_concurrency bound, on a
	// dedicated agent instance when that bound is above 1
	// (T-20261107-plan-fan-out-steps-run-one-at-a-time-on-a). The planner never emits it.
	FanOutItem string `json:"fan_out_item,omitempty"`

	// FanOutItemJSON is the child's whole item as compact JSON (ADR-0145 D6.3): the
	// source element FanOutItem labels — a record with its amount, date and id — or
	// the JSON string of a scalar item. Stamped on the child's handoff as
	// HandoffKeyFanOutItem. Set only by ExpandFanOut/ResolveFanOut.
	FanOutItemJSON string `json:"fan_out_item_json,omitempty"`

	// Answers lists the requirement checklist items this step produces results
	// for (ADR-0145 D6: "R1", "R2"; RequirementChecklist). The plan validator checks
	// they name real items and that every item has a producing step; the executor
	// asks the step's worker for the answer envelope for exactly these items. Set
	// by the planner only under execution.plan.checklist_enabled.
	Answers []string `json:"answers,omitempty"`
	// Inputs are bindings to earlier steps' values (ADR-0145 D6): "s0" (the whole
	// output of step 0), "s0.overdue" (the values step 0's envelope named
	// "overdue"), with an optional "[*]" for a list. Every binding names a step this
	// one depends on (plan validator). The executor resolves them from the ledger
	// and hands them to the worker (HandoffKeyStepInputs).
	Inputs []string `json:"inputs,omitempty"`
	// ForEach ties the step's items to a set found earlier ("s0.overdue"): code
	// then requires one result per member of the set (ADR-0145 D5.6). On a
	// fan-out step it is implied by fan_out_over.
	ForEach string `json:"for_each,omitempty"`
}

// HandoffKeyStepInputs carries a step's resolved input bindings (Step.Inputs) as
// JSON []StepInputValue. Kernel-authored, like HandoffKeyStepDependencies.
const HandoffKeyStepInputs = "_step_inputs"

// HandoffKeyFanOutItem is the handoff context key a fan-out child receives its
// whole item object under (Step.FanOutItemJSON). Kernel-authored: stamped on a
// child's dispatch, removed from every other step's.
const HandoffKeyFanOutItem = "_fan_out_item"

// ExecutionPlan carries the structured plan produced by the Planner.
type ExecutionPlan struct {
	Steps         []Step         `json:"steps"`
	Subject       string         `json:"subject"`
	CachePolicy   string         `json:"cache_policy,omitempty"` // ADR-0027: LLM-classified policy name for Hippocampus retrieval thresholds
	PlanningFacts []SearchResult `json:"-"`                      // AGENTCONTEXTREQ: planning-time LTM facts forwarded to agents; not serialised in JSON prompt.
	// PROMPTREQ: hash of the static prompt template that produced this plan; written to PlanEvent.
	PlannerPromptVersion string `json:"-"`
	// FollowedProcedures are the ADR-0094 routine IDs that were in the planner's
	// context when this plan was produced (ADR-0094 D8 co-evolution).
	//
	// It closes the tier's learning loop. PlanRecord.FollowedProcedures was declared
	// and READ by the memory agent — which feeds each routine's confidence from the
	// outcome and deprecates the ones that stop working — but nothing ever wrote it,
	// so `len(rec.FollowedProcedures) > 0` was never true and no routine's confidence
	// ever moved. The tier could influence a plan and never learn whether it helped.
	FollowedProcedures []string `json:"-"`
}

// Clone returns a deep copy of the ExecutionPlan.
// PlanningFacts are omitted because they are session-specific and not
// serialised to the Hippocampus (json:"-"); the cloned plan starts fresh.
func (e *ExecutionPlan) Clone() *ExecutionPlan {
	if e == nil {
		return nil
	}
	cloned := &ExecutionPlan{
		Subject:              e.Subject,
		CachePolicy:          e.CachePolicy,
		PlannerPromptVersion: e.PlannerPromptVersion,
	}
	// Provenance must survive the freeze, or a replanned plan loses the record of
	// which routines informed it and their confidence never updates.
	if len(e.FollowedProcedures) > 0 {
		cloned.FollowedProcedures = append([]string(nil), e.FollowedProcedures...)
	}
	if len(e.Steps) > 0 {
		cloned.Steps = make([]Step, len(e.Steps))
		for i, s := range e.Steps {
			cloned.Steps[i] = Step{
				Query:           s.Query,
				IsThought:       s.IsThought,
				MaxEnergy:       s.MaxEnergy,
				CheckpointAfter: s.CheckpointAfter,
				CacheTTLSeconds: s.CacheTTLSeconds,
				FanOutVar:       s.FanOutVar,
				FanOutItem:      s.FanOutItem,
				FanOutItemJSON:  s.FanOutItemJSON,
				// This clone is field-by-field, so a field omitted here vanishes on
				// every replan and plan freeze without a compiler error — the same
				// silent-drop shape that made the EFE arm exempt from the capability
				// contract. Any field added to Step must be added here too.
				PreferredAgent: s.PreferredAgent,
				AgentPin:       s.AgentPin,
			}
			if s.FanOutOver != nil { // deep-copy the pointer so a clone never aliases
				v := *s.FanOutOver
				cloned.Steps[i].FanOutOver = &v
			}
			if len(s.DependsOn) > 0 {
				cloned.Steps[i].DependsOn = make([]int, len(s.DependsOn))
				copy(cloned.Steps[i].DependsOn, s.DependsOn)
			}
			if len(s.RequiredCapabilities) > 0 {
				cloned.Steps[i].RequiredCapabilities = make([]string, len(s.RequiredCapabilities))
				copy(cloned.Steps[i].RequiredCapabilities, s.RequiredCapabilities)
			}
			if s.RequiredEffects != nil { // nil vs empty is meaningful (declared vs legacy)
				cloned.Steps[i].RequiredEffects = append([]string{}, s.RequiredEffects...)
			}
			cloned.Steps[i].Answers = append([]string(nil), s.Answers...)
			cloned.Steps[i].Inputs = append([]string(nil), s.Inputs...)
			cloned.Steps[i].ForEach = s.ForEach
		}
	}
	return cloned
}
