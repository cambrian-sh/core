package domain

// AgentManager manages the runtime lifecycle of agent instances.
// It is strictly lifecycle-only: boot, evict, find. (Restore — workspace
// rollback, ADR-0005 D2b/D3 — was removed by owner decision 2026-10-31.)
// TaskEventWriter and cost config are injected separately by the kernel.
type AgentManager interface {
	GetInstanceIDs(agentID string) []string
	FindInstanceByToken(token string) *Instance
	EvictInstance(instanceID string)
}
