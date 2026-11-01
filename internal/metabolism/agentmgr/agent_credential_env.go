package agentmgr

// The agent's credential, placed in its process environment at spawn
// (ADR-0130 D1).
//
// SEC-01 built a deny-by-default environment so no kernel secret reaches an
// agent process (env_allowlist.go strips anything credential-shaped, and
// CAMBRIAN_* wholesale). The agent-plane credential is the ONE deliberate
// exception: it is the agent's OWN secret, minted for it and nothing else, and
// the SDK reads it from exactly these two variables. They are appended after
// the allowlist filter on purpose — the filter is what keeps the kernel's
// secrets out, and this is not one of them.

// AgentTokenEnv carries the agent-plane bearer (ADR-0130 D1). The SDK
// (cambrian_agent_sdk/channel.py) presents it as `authorization: Bearer`.
const AgentTokenEnv = "CAMBRIAN_AGENT_TOKEN"

// AgentIDEnv names the agent the token was issued to, so every RPC the SDK
// makes can carry the matching x-agent-id assertion — including the ones that
// did not set it before ADR-0130 (SignalStream, context-node reads).
const AgentIDEnv = "CAMBRIAN_AGENT_ID"

// SetAgentCredentialSource wires the per-agent credential lookup. Called once
// by the composition root; nil (the default, and every test) spawns agents
// with no credential, which is correct on a waived plane and refused with a
// named reason on a strict one.
func (im *InstanceManager) SetAgentCredentialSource(resolve func(agentID string) (string, bool)) {
	im.agentCredential = resolve
}

// agentCredentialEnv returns the KEY=VALUE pairs for one agent, or nothing when
// no credential is issued. The id rides alongside the token so the SDK never
// has to guess which agent a token belongs to.
func (im *InstanceManager) agentCredentialEnv(agentID string) []string {
	if im.agentCredential == nil {
		return nil
	}
	tok, ok := im.agentCredential(agentID)
	if !ok || tok == "" {
		return nil
	}
	return []string{AgentTokenEnv + "=" + tok, AgentIDEnv + "=" + agentID}
}
