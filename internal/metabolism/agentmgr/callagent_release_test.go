package agentmgr

import (
	"context"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// The 3-arg CallAgent (premium reactive, ingress studio) must not kill a running
// JIT instance it merely REUSED: that instance may be the one the agent-plane
// Transport caches, and killing it tore down the Transport's live agent
// (T-20261113-agentmanager-callagent-kills-the-shared-jit).
func TestCallAgent_DoesNotReleaseAReusedInstance(t *testing.T) {
	m := newTestManager()
	reg := m.Registry.(*testRegistry)
	reg.agents["shared_agent"] = domain.AgentDefinition{ID: "shared_agent", Name: "shared_agent"}

	shared := domain.NewInstance("shared_agent") // ModeJIT, as the Transport boots it
	shared.SocketPath = t.TempDir() + "/absent.sock"
	im := m.InstanceManager
	im.mu.Lock()
	im.instances[shared.ID] = shared
	im.agentIndex["shared_agent"] = []string{shared.ID}
	im.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The socket does not exist, so the call itself fails; what matters is that
	// the instance it found is still tracked afterwards.
	_, _ = m.CallAgent(ctx, "shared_agent", &domain.Handoff{})

	if got, ok := im.GetInstanceByID(shared.ID); !ok || got != shared {
		t.Fatal("CallAgent released (killed) an instance it did not boot")
	}
}
