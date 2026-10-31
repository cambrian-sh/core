package agentmgr

import (
	"github.com/cambrian-sh/core/internal/metabolism/connector"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// AgentConnector manages gRPC connections and A2A protocol clients for agent
// sessions. It used to also hold CoW git snapshots of agent dirs (ADR-0005
// D2b); those were removed by owner decision 2026-10-31.
type AgentConnector struct {
	a2aClient *connector.A2AClient
}

// NewAgentConnector creates an AgentConnector with default wiring.
func NewAgentConnector() *AgentConnector {
	return &AgentConnector{
		a2aClient: connector.New(),
	}
}

func (ac *AgentConnector) getA2AClient() *connector.A2AClient { return ac.a2aClient }

// DialAgent connects to the specified agent via its address.
func (ac *AgentConnector) DialAgent(addr string) (*grpc.ClientConn, error) {
	// Use grpc.NewClient (non-blocking) instead of the deprecated grpc.Dial
	// with WithBlock. Blocking dials can hang indefinitely on Windows UDS
	// when the agent socket exists but the gRPC server is not yet serving.
	// NewClient defers connection establishment to the first RPC, which
	// respects the RPC's context deadline.
	return grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
}
