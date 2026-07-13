package agentmgr

import (
	"context"
	"fmt"
	"sync"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/metabolism/connector"
	"github.com/cambrian-sh/core/internal/substrate/harness"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// AgentConnector manages gRPC connections, A2A protocol clients,
// and CoW filesystem snapshots for agent sessions.
type AgentConnector struct {
	a2aClient *connector.A2AClient

	snapshotMu sync.Mutex
	snapshots  map[string]string // key: agentID+taskID → git commit hash
}

// NewAgentConnector creates an AgentConnector with default wiring.
func NewAgentConnector() *AgentConnector {
	return &AgentConnector{
		a2aClient: connector.New(),
		snapshots: make(map[string]string),
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

// Restore resets the agent's working directory to the git state captured
// at the snapshot hash. Returns nil if no snapshot exists (graceful no-op).
func (ac *AgentConnector) Restore(registry domain.AgentResolver, agentID, taskID string) error {
	key := agentID + taskID

	ac.snapshotMu.Lock()
	hash := ac.snapshots[key]
	ac.snapshotMu.Unlock()
	ctx := context.Background()
	if hash == "" {
		return nil
	}

	def, err := registry.GetAgentByName(ctx, agentID)
	if err != nil {
		ac.snapshotMu.Lock()
		delete(ac.snapshots, key)
		ac.snapshotMu.Unlock()
		return fmt.Errorf("Restore: agent lookup failed: %w", err)
	}

	restoreErr := harness.Restore(def.Dir, hash)

	ac.snapshotMu.Lock()
	delete(ac.snapshots, key)
	ac.snapshotMu.Unlock()

	return restoreErr
}
