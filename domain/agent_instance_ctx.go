package domain

import "context"

// The agent INSTANCE a call came from (ADR-0157 D5.2). The agent-plane verifier
// stamps it when the caller presented a live per-instance token (D4): the token
// names exactly one (agent, instance) pair the kernel minted at spawn, so the
// instance is a kernel-held fact, never a field of a request. Absent for an
// external agent authenticated by its stored secret, for the waiver path and for
// in-process calls. Provenance writers (UploadArtifact, the out/ collection)
// read it; nothing authorizes on it.

type agentInstanceCtxKey struct{}

// WithAgentInstance returns a child context naming the calling agent instance.
func WithAgentInstance(ctx context.Context, instanceID string) context.Context {
	if instanceID == "" {
		return ctx
	}
	return context.WithValue(ctx, agentInstanceCtxKey{}, instanceID)
}

// AgentInstanceFromContext returns the calling agent instance, "" when the
// verifier did not establish one.
func AgentInstanceFromContext(ctx context.Context) string {
	id, _ := ctx.Value(agentInstanceCtxKey{}).(string)
	return id
}

// AgentOutputs is one exited instance's out/ folder (ADR-0157 D5.1): the kernel
// launched the instance, so agent and instance are kernel facts.
type AgentOutputs struct {
	AgentID    string
	InstanceID string
	// Dir is the out/ folder on the host, inside the instance's scratch.
	Dir string
}

// AgentOutputCollector records an exited instance's out/ files as run-ledger
// artifacts before its scratch is removed. It returns the files recorded; an
// error names what was not (a file over a cap, a write that failed).
type AgentOutputCollector interface {
	CollectAgentOutputs(ctx context.Context, out AgentOutputs) (int, error)
}
