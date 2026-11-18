package agentmgr

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0157 D5.1 — artifacts leave an agent only through the kernel. A sandboxed
// instance gets an out/ folder in its private scratch (CAMBRIAN_AGENT_OUT). When
// the instance exits — on its own or because the kernel ended it — the kernel
// collects what is there into the run ledger BEFORE the scratch is removed;
// everything else the agent wrote is deleted with the scratch. The other exit is
// the UploadArtifact RPC. Both record through the ledger's classified writer.

// AgentOutEnv names the out/ folder in the agent's environment.
const AgentOutEnv = "CAMBRIAN_AGENT_OUT"

// AgentOutDir is the out/ folder's path relative to the instance scratch.
const AgentOutDir = "out"

// collectTimeout bounds one instance's out/ collection at exit.
const collectTimeout = 2 * time.Minute

// SetOutputCollector wires the ledger-side collector of instance out/ folders
// (app composition). Without one, out/ is deleted with the scratch like every
// other file: nothing leaves, so a missing collector loses files, it never
// leaks them — which is why it may be wired after construction.
func (im *InstanceManager) SetOutputCollector(c domain.AgentOutputCollector) {
	im.mu.Lock()
	defer im.mu.Unlock()
	im.outputs = c
}

// collectOutputs hands an exited instance's out/ to the collector.
func (im *InstanceManager) collectOutputs(ctx context.Context, agentID, instanceID, workdir string) {
	if workdir == "" {
		return
	}
	im.mu.Lock()
	c := im.outputs
	im.mu.Unlock()
	if c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, collectTimeout)
	defer cancel()
	n, err := c.CollectAgentOutputs(ctx, domain.AgentOutputs{
		AgentID: agentID, InstanceID: instanceID, Dir: filepath.Join(workdir, AgentOutDir),
	})
	if err != nil {
		slog.Warn("ADR-0157: agent out/ collection incomplete", "agent", agentID, "instance", instanceID,
			"recorded", n, "err", err)
		return
	}
	if n > 0 {
		slog.Info("ADR-0157: agent out/ collected into the run ledger", "agent", agentID, "instance", instanceID, "files", n)
	}
}

// handoffLeaseKey is the handoff context key the kernel puts a dispatch's lease
// under (the same key internal/metabolism/dispatch and the network lanes stamp,
// and the SDK reads as the task's session_token_id).
const handoffLeaseKey = "_session_token_id"

// NoteDispatch tells the output collector, when it observes dispatches
// (domain.AgentWorkObserver), that instanceID of agentID is about to work under
// the lease on h. Called by every path that sends a handoff to a chosen instance
// (CallAgent, CallDaemon, the agent-plane Transport), so an instance's uploads
// and out/ carry the run of the work the kernel gave it (ADR-0157 D5.2).
func (im *InstanceManager) NoteDispatch(agentID, instanceID string, h *domain.Handoff) {
	if instanceID == "" || h == nil {
		return
	}
	lease := domain.LeaseID(h.Context[handoffLeaseKey])
	if lease == "" {
		return
	}
	im.mu.Lock()
	c := im.outputs
	im.mu.Unlock()
	if o, ok := c.(domain.AgentWorkObserver); ok {
		o.AgentInstanceDispatched(agentID, instanceID, lease)
	}
}
