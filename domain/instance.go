package domain

import "github.com/google/uuid"

// InstanceMode is the runtime scaling mode of an agent instance.
type InstanceMode string

const (
	ModeJIT    InstanceMode = "jit"
	ModePool   InstanceMode = "pool"
	ModeDaemon InstanceMode = "daemon"
)

// Instance is a single running agent process — the Phenotype.
// Multiple Instances can exist for one AgentDefinition (Genotype).
type Instance struct {
	ID         string
	AgentID    string
	SocketPath string
	Mode       InstanceMode
	// No credential lives here: the instance's agent-plane token is held only
	// by the kernel's token registry (authz.InstanceTokens, ADR-0157 D4), so
	// there is one copy to revoke.
}

// NewInstance creates a tracked Instance with a unique UUID.
// The default Mode is ModeJIT.
func NewInstance(agentID string) *Instance {
	return &Instance{
		ID:      uuid.New().String(),
		AgentID: agentID,
		Mode:    ModeJIT,
	}
}

// AgentInstanceInfo is one live agent process as the operator plane lists it
// (OperatorConsole/ListAgentInstances, ENG-1634): which agent, which instance,
// the daemon stream it serves ("" for a JIT instance), and the OS process id —
// what an operator (and the chaos gate's external SIGKILL) needs to find the
// process outside the kernel. PID is 0 when the process handle is not known.
type AgentInstanceInfo struct {
	AgentID    string
	InstanceID string
	StreamID   string
	Mode       InstanceMode
	PID        int
}
