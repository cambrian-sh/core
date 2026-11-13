package compose

import (
	"context"
	"errors"
	"log/slog"

	"github.com/cambrian-sh/core/domain"
)

// ErrNoProfessions is ComposeRequest's refusal when no profession is registered:
// there is nothing to compose the request's worker from.
var ErrNoProfessions = errors.New("compose: no profession profiles are registered")

// ComposeRequest composes the worker of a whole REQUEST for the ADR-0150
// single-agent lane (execution.lane.single_agent.agent_id empty): one runtime,
// the skills of every registered profession, and every tool inside their
// allowlists that the act-for person's scope admits (ADR-0150 D1: "the tools the
// person's grants allow, discovered within the person's access", ADR-0145
// D14.2). There is no step and no selection: the lane has no planner, so the
// one agent holds the whole permitted set and the model narrows it (it may never
// widen it: the composition is bound to the lease and enforced by the tool
// executor's lease ceiling, as for a plan step's composition).
//
// The composition is bound to lease, its tools activated as the lease's
// run-scoped grants, stamped on the handoff and recorded in the ledger, exactly
// as Compose does for a step. It returns the runtime's agent id to dispatch to.
func (c *Composer) ComposeRequest(ctx context.Context, lease domain.LeaseID, brief string, in *domain.Handoff) (string, domain.WorkerComposition, error) {
	if c == nil || c.Leases == nil || c.Agents == nil || c.Manifests == nil {
		return "", domain.WorkerComposition{}, errors.New("compose: composer not wired")
	}
	if lease == "" {
		return "", domain.WorkerComposition{}, errors.New("compose: no lease to bind the request's composition to")
	}
	if prior, bound := c.Leases.For(string(lease)); bound {
		// A retried dispatch on the same lease keeps its composition.
		return prior.Runtime, prior, nil
	}
	runtime, err := c.Agents.GetAgentByName(ctx, c.RuntimeID)
	if err != nil || runtime == nil {
		return "", domain.WorkerComposition{}, errors.New("compose: the composed runtime agent " + c.RuntimeID + " is not registered")
	}
	if c.Fleet == nil {
		return "", domain.WorkerComposition{}, errors.New("compose: the agent registry cannot list professions")
	}
	agents, err := c.Fleet.GetAllAgents(ctx)
	if err != nil {
		return "", domain.WorkerComposition{}, err
	}
	var professions []domain.AgentDefinition
	var skills, allow []string
	for _, a := range agents {
		if a.Profile == "" {
			continue
		}
		m, mErr := c.Manifests.GetManifest(ctx, a.ID)
		if mErr != nil || m == nil {
			continue
		}
		professions = append(professions, a)
		skills = append(skills, m.Skills...)
		allow = append(allow, m.ToolAllowlist...)
		if c.Grants != nil {
			if set, ok := c.Grants.Effective(ctx, a.ID); ok {
				for name, acc := range set {
					if acc == domain.SkillAccessGranted || acc == domain.SkillAccessProfile {
						skills = append(skills, name)
					}
				}
			}
		}
	}
	if len(professions) == 0 {
		return "", domain.WorkerComposition{}, ErrNoProfessions
	}
	sortAgents(professions)
	task := &domain.DispatchTask{ID: "request", Description: brief}
	comp := domain.WorkerComposition{
		Runtime:      runtime.ID,
		Skills:       c.indexSkills(skills),
		Tools:        c.tools(ctx, runtime.ID, domain.ToolAllowlist(allow), task),
		Brief:        brief,
		Model:        runtime.ModelPreference,
		SystemPrompt: c.systemPrompt(),
		StepIndex:    stepIndex(in),
	}
	for _, p := range professions {
		comp.Professions = append(comp.Professions, p.ID)
	}
	prov := domain.LedgerProvenanceFromContext(ctx)
	comp.RunID = prov.RunID
	comp = comp.Normalize()

	c.Leases.Bind(lease, comp)
	if c.Overlay != nil && len(comp.Tools) > 0 {
		c.Overlay.Activate(string(lease), comp.Tools)
	}
	if in != nil {
		if in.Context == nil {
			in.Context = map[string]string{}
		}
		in.Context[domain.HandoffKeyComposition] = comp.Encode()
		in.Context[HandoffKeyCompositionID] = comp.ID()
	}
	c.record(ctx, prov, comp)
	slog.InfoContext(ctx, "ADR-0150 D1: request composed for the single-agent lane",
		"composition", comp.ID(), "runtime", comp.Runtime, "professions", len(comp.Professions),
		"skills", len(comp.Skills), "tools", len(comp.Tools))
	return runtime.ID, comp, nil
}
