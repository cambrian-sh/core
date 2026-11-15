package domain

import "context"

// Agent-plane proposal verbs (owner decision 2026-11-13, option A;
// T-20261113-agent-proposal-rpcs-proposeknowledge). An agent may call
// Orchestrator/ProposeKnowledge or Orchestrator/ProposeOntologyChange only when
// the kernel says so — the SDK's allow_*_proposals flags only decide whether the
// verb is offered to the model, and an agent calling the RPC directly skips them.
//
// The kernel decides with the per-agent tool-grant mechanism, under the verb's
// name, in the order the tool-grant check uses (ToolExecutor.grantFor): the tool
// ceilings and the lease's ceiling first, then the tools_unrestricted bypass,
// then the agent's grants (an operator's SetToolGrant names the verb) and a
// skill's run overlay. One addition: a profile agent's `tools:` allowlist names
// the verb. Profile grants are expanded only over REGISTERED tools and a verb is
// not one, so the profile's own glob is read directly — the same ceiling that
// already bounds it.

// Proposal verbs, as named in grants and profile `tools:` entries.
const (
	VerbProposeKnowledge      = "propose_knowledge"
	VerbProposeOntologyChange = "propose_ontology_change"
)

// AgentVerbAllowed reports whether agentID, acting under the lease (session
// token) sessionToken, may use the kernel verb. Fails closed: an anonymous
// agent, a nil executor, or an executor without a grants store in restricted
// mode is refused.
func (e *ToolExecutor) AgentVerbAllowed(ctx context.Context, agentID, verb, sessionToken string) bool {
	if e == nil || agentID == "" || verb == "" {
		return false
	}
	if e.unrestricted || e.Grants != nil {
		if _, ok := e.grantFor(ctx, agentID, verb, sessionToken); ok {
			return true
		}
	}
	// A profile `tools:` entry naming the verb (see above). The ceilings still
	// hold: inside every ceiling that binds the agent and the lease's.
	if e.ceiling == nil {
		return false
	}
	inside, bound := e.ceiling.Allows(agentID, verb)
	return bound && inside && e.withinCeiling(agentID, verb) && e.WithinLeaseCeiling(sessionToken, verb)
}
