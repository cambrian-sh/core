package domain

type Step struct {
	Query            string  `json:"query"`
	DependsOn        []int   `json:"depends_on,omitempty"`
	IsThought        bool    `json:"is_thought,omitempty"`
	MaxEnergy        float64 `json:"max_energy,omitempty"`
	RecommendedModel string  `json:"recommended_model,omitempty"`
	CheckpointAfter  bool    `json:"checkpoint_after,omitempty"`
	CheckpointQuery  string  `json:"checkpoint_query,omitempty"`
	CacheTTLSeconds  int     `json:"cache_ttl_seconds,omitempty"`
}

// ExecutionPlan carries the structured plan produced by the Planner.
type ExecutionPlan struct {
	Steps                []Step         `json:"steps"`
	Subject              string         `json:"subject"`
	CachePolicy          string         `json:"cache_policy,omitempty"`  // ADR-0027: LLM-classified policy name for Hippocampus retrieval thresholds
	PlanningFacts        []SearchResult `json:"-"` // AGENTCONTEXTREQ: planning-time LTM facts forwarded to agents; not serialised in JSON prompt.
	PlannerPromptVersion string         `json:"-"` // PROMPTREQ: hash of the static prompt template that produced this plan; written to PlanEvent.
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
			}
			if len(s.DependsOn) > 0 {
				cloned.Steps[i].DependsOn = make([]int, len(s.DependsOn))
				copy(cloned.Steps[i].DependsOn, s.DependsOn)
			}
		}
	}
	return cloned
}
