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

// The agent's TRANSPORT to the kernel (ADR-0130 D3/D4;
// T-20261101-kernel-spawned-agents-dial-localhost-port-and).
//
// The metabolism stack builds the manager with "localhost:<port>", which is
// right only when the kernel listens on loopback in plaintext or in the
// loopback TLS+plaintext demux. On a routable bind nothing listens on
// localhost, and in pure-TLS mode a plaintext dial is refused. The composition
// root therefore resolves the address and the TLS facts from the ONE listener
// policy (app/listener_security.go) and hands them here, next to the
// credential — the policy decision stays in one place and this package only
// carries it to the process environment.

// TLSCAEnv names the PEM root the SDK verifies the kernel against; set ⇒ the
// SDK dials TLS (cambrian_agent_sdk/channel.py TLS_CA_ENV).
const TLSCAEnv = "CAMBRIAN_TLS_CA"

// TLSServerNameEnv overrides the name the SDK verifies the kernel's
// certificate against (channel.py TLS_SERVER_NAME_ENV) — used when the agent
// dials an address the certificate does not name (a wildcard bind reached on
// loopback).
const TLSServerNameEnv = "CAMBRIAN_TLS_SERVER_NAME"

// AgentTransport is how a spawned agent reaches the kernel.
type AgentTransport struct {
	// Addr is the host:port passed as --substrate-addr. Empty keeps the
	// address the manager was constructed with.
	Addr string
	// TLSCAFile, when set, is injected as CAMBRIAN_TLS_CA: the agent dials TLS
	// and verifies against this root.
	TLSCAFile string
	// TLSServerName, when set, is injected as CAMBRIAN_TLS_SERVER_NAME.
	TLSServerName string
}

// SetAgentTransport wires the dial facts every spawn uses. Called once by the
// composition root before any agent spawns. It is transport plumbing, not a
// security control: the kernel's listener enforces TLS and the agent
// credential whether or not an agent was told how to satisfy them.
func (im *InstanceManager) SetAgentTransport(t AgentTransport) {
	if t.Addr != "" {
		im.substrateAddr = t.Addr
	}
	im.agentTLSCA = t.TLSCAFile
	im.agentTLSServerName = t.TLSServerName
}

// agentTransportEnv returns the TLS KEY=VALUE pairs for a spawn. Like the
// credential, they are appended AFTER the SEC-01 allowlist (which strips
// CAMBRIAN_* wholesale): they are the kernel telling its own agent how to
// reach it, not a kernel secret — a CA certificate is public material.
func (im *InstanceManager) agentTransportEnv() []string {
	var out []string
	if im.agentTLSCA != "" {
		out = append(out, TLSCAEnv+"="+im.agentTLSCA)
		if im.agentTLSServerName != "" {
			out = append(out, TLSServerNameEnv+"="+im.agentTLSServerName)
		}
	}
	return out
}
