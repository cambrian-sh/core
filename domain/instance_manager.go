package domain

// AgentManager manages the runtime lifecycle of agent instances.
// It is strictly lifecycle-only: boot, evict, find, restore.
// TaskEventWriter and cost config are injected separately by the kernel.
type AgentManager interface {
	GetInstanceIDs(agentID string) []string
	FindInstanceByToken(token string) *Instance
	EvictInstance(instanceID string)
	Restore(agentID, taskID string) error
}
