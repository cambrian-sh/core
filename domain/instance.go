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
	AuthToken  string
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
