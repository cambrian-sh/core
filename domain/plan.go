package domain

// Agent-pin strengths for Step.AgentPin / AuctionTask.AgentPin.
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
	// PinHard binds the named agent directly, skipping auction and semantic gating.
	// Reserved for an imperative user instruction ("use X to do Y"), where silently
	// routing elsewhere would be a worse answer than failing. An unavailable name is
	// an error, not a fallback.
	PinHard = "hard"
)

type Step struct {
	Query            string  `json:"query"`
	DependsOn        []int   `json:"depends_on,omitempty"`
	IsThought        bool    `json:"is_thought,omitempty"`
	MaxEnergy        float64 `json:"max_energy,omitempty"`
	RecommendedModel string  `json:"recommended_model,omitempty"`
	CheckpointAfter  bool    `json:"checkpoint_after,omitempty"`
	CheckpointQuery  string  `json:"checkpoint_query,omitempty"`
	CacheTTLSeconds  int     `json:"cache_ttl_seconds,omitempty"`
	// RequiredCapabilities is the ROUTE-03 capability contract: the capability
	// tags a step needs its executor to declare, emitted by the planner from the
	// live capability-cluster vocabulary. When non-empty AND the capability
	// contract is enabled, L1 Declaration hard-gates candidates on
	// required ⊆ manifest.Capabilities. Empty ⇒ today's behavior (backward
	// compatible). Populated only under the capability_contract arm.
	RequiredCapabilities []string `json:"required_capabilities,omitempty"`

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
}

// ExecutionPlan carries the structured plan produced by the Planner.
type ExecutionPlan struct {
	Steps                []Step         `json:"steps"`
	Subject              string         `json:"subject"`
	CachePolicy          string         `json:"cache_policy,omitempty"` // ADR-0027: LLM-classified policy name for Hippocampus retrieval thresholds
	PlanningFacts        []SearchResult `json:"-"`                      // AGENTCONTEXTREQ: planning-time LTM facts forwarded to agents; not serialised in JSON prompt.
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
				Query:            s.Query,
				IsThought:        s.IsThought,
				MaxEnergy:        s.MaxEnergy,
				RecommendedModel: s.RecommendedModel,
				CheckpointAfter:  s.CheckpointAfter,
				CheckpointQuery:  s.CheckpointQuery,
				CacheTTLSeconds:  s.CacheTTLSeconds,
				FanOutVar:        s.FanOutVar,
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
		}
	}
	return cloned
}
