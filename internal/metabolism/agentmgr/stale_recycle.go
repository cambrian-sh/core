package agentmgr

import (
	"context"
	"log/slog"

	"github.com/cambrian-sh/core/domain"
)

// Stale-instance recycle (ADR-0157 D7.10.2, A1-Q11, P4).
//
// A running instance keeps the Python layer generation it started with: its
// sys.path was fixed at start. When an agent's layer changes (an Operator
// approved a runtime package request, or a declared package's install landed),
// the package service calls MarkAgentStale; every live instance of the agent is
// then recycled when it is idle — after its current Execute — through the
// existing eviction path, so the change is usable within one task and no work
// in flight is killed. The rejected alternative (no recycle; the next natural
// spawn picks it up) leaves a long-lived pool worker or shared JIT instance on
// the old generation indefinitely.
//
// Idle is counted here, not guessed: every Execute through the kernel's three
// dispatch paths (agentplane Transport.CallAgent, AgentManager.CallAgent,
// AgentManager.CallDaemon) is bracketed by ExecuteBegan / ExecuteEnded. An
// instance chosen for recycle is "retiring": no new Execute is admitted on it
// (ExecuteBegan refuses it, so the caller dials another) and GetOrBootInstance
// no longer returns it.

// staleState is the per-instance recycle bookkeeping (guarded by im.mu).
type staleState struct {
	stale    map[string]bool // instanceID → its layer generation is out of date
	inflight map[string]int  // instanceID → Executes in progress
	retiring map[string]bool // instanceID → chosen for recycle; admits nothing new
}

func (s *staleState) init() {
	if s.stale == nil {
		s.stale = map[string]bool{}
		s.inflight = map[string]int{}
		s.retiring = map[string]bool{}
	}
}

func (s *staleState) forget(instanceID string) {
	if s.stale == nil {
		return
	}
	delete(s.stale, instanceID)
	delete(s.inflight, instanceID)
	delete(s.retiring, instanceID)
}

// markAgentStale marks every live instance of agentID stale and returns the
// ids of those that are idle right now (no Execute in progress), now retiring.
func (im *InstanceManager) markAgentStale(agentID string) (idle []*domain.Instance, marked int) {
	im.mu.Lock()
	defer im.mu.Unlock()
	im.recycle.init()
	for _, id := range im.agentIndex[agentID] {
		inst, ok := im.instances[id]
		if !ok {
			continue
		}
		im.recycle.stale[id] = true
		marked++
		if im.recycle.inflight[id] == 0 && !im.recycle.retiring[id] {
			im.recycle.retiring[id] = true
			idle = append(idle, inst)
		}
	}
	return idle, marked
}

// isRetiringLocked reports whether instanceID was chosen for recycle. Caller holds im.mu.
func (im *InstanceManager) isRetiringLocked(instanceID string) bool {
	return im.recycle.retiring != nil && im.recycle.retiring[instanceID]
}

// beginExecute admits one Execute on instanceID: false when the instance is
// gone or retiring (the caller dials another instance).
func (im *InstanceManager) beginExecute(instanceID string) bool {
	im.mu.Lock()
	defer im.mu.Unlock()
	if _, ok := im.instances[instanceID]; !ok {
		return false
	}
	im.recycle.init()
	if im.recycle.retiring[instanceID] {
		return false
	}
	im.recycle.inflight[instanceID]++
	return true
}

// endExecute ends one Execute on instanceID. It returns the instance when it
// is stale and now idle, marked retiring (the caller recycles it).
func (im *InstanceManager) endExecute(instanceID string) *domain.Instance {
	im.mu.Lock()
	defer im.mu.Unlock()
	im.recycle.init()
	if n := im.recycle.inflight[instanceID]; n > 1 {
		im.recycle.inflight[instanceID] = n - 1
		return nil
	}
	delete(im.recycle.inflight, instanceID)
	inst, ok := im.instances[instanceID]
	if !ok || !im.recycle.stale[instanceID] || im.recycle.retiring[instanceID] {
		return nil
	}
	im.recycle.retiring[instanceID] = true
	return inst
}

// MarkAgentStale is told the agent's Python layer changed (pypackages
// Config.OnLayerChange): each live instance is recycled when idle — the idle
// ones now, a busy one after its current Execute.
func (m *AgentManager) MarkAgentStale(agentID string) {
	idle, marked := m.InstanceManager.markAgentStale(agentID)
	if marked == 0 {
		return
	}
	slog.Info("ADR-0157 D7.10: agent's python layer changed; recycling its instances when idle",
		"agent_id", agentID, "instances", marked, "idle_now", len(idle))
	for _, inst := range idle {
		m.recycle(inst)
	}
}

// ExecuteBegan admits one Execute on instanceID (false: it is retiring or gone;
// dial another instance). Every true is paired with one ExecuteEnded.
func (m *AgentManager) ExecuteBegan(instanceID string) bool {
	return m.InstanceManager.beginExecute(instanceID)
}

// ExecuteEnded ends one Execute on instanceID. When the instance is stale and
// now idle it is recycled, and the result is true: the caller drops any cached
// connection to it.
func (m *AgentManager) ExecuteEnded(instanceID string) bool {
	inst := m.InstanceManager.endExecute(instanceID)
	if inst == nil {
		return false
	}
	m.recycle(inst)
	return true
}

// recycle replaces a retiring instance: a JIT instance is evicted (the next
// call boots a fresh one on the agent's current layer); a daemon is evicted
// and its stream re-spawned with the same params and references, the way the
// heartbeat reclaim does it (reclaimHungDaemon) but without a crash.
func (m *AgentManager) recycle(inst *domain.Instance) {
	slog.Info("ADR-0157 D7.10: recycling a stale agent instance", "agent_id", inst.AgentID,
		"instance_id", inst.ID, "mode", inst.Mode)
	if inst.Mode == domain.ModeDaemon && m.recycleDaemon(inst.ID) {
		return
	}
	m.EvictInstance(inst.ID)
}

// recycleDaemon re-spawns the daemon stream instanceID serves. It returns false
// when no stream owns the instance (the caller just evicts it). The stream is
// marked booting in the same critical section that unregisters it, so a
// CallDaemon or SpawnDaemon meanwhile waits for the new process instead of
// reading the worker as lost.
func (m *AgentManager) recycleDaemon(instanceID string) bool {
	m.daemons.mu.Lock()
	var stream string
	var st *daemonState
	for s, d := range m.daemons.byStream {
		if d.instanceID == instanceID {
			stream, st = s, d
			break
		}
	}
	if st == nil {
		m.daemons.mu.Unlock()
		return false
	}
	if _, inFlight := m.daemons.booting[stream]; inFlight {
		m.daemons.mu.Unlock()
		return true // someone is already replacing it
	}
	booted := make(chan struct{})
	m.daemons.booting[stream] = booted
	delete(m.daemons.byStream, stream)
	m.daemons.mu.Unlock()

	m.markExpectedStop(instanceID)
	m.EvictInstance(instanceID)

	stop, ok := m.restarts.begin()
	if !ok { // shutting down: leave the stream down
		m.daemons.mu.Lock()
		delete(m.daemons.booting, stream)
		close(booted)
		m.daemons.mu.Unlock()
		return true
	}
	go func() {
		defer m.restarts.wg.Done()
		select {
		case <-stop:
			m.daemons.mu.Lock()
			delete(m.daemons.booting, stream)
			close(booted)
			m.daemons.mu.Unlock()
			return
		default:
		}
		// A kernel-owned replacement: no request started it (like scheduleRestart).
		if _, err := m.bootClaimedDaemon(context.Background(), st.agentID, stream, st.params, booted, st.refCount); err != nil {
			slog.Warn("ADR-0157 D7.10: stale daemon recycle failed to boot; handing the stream to the restart policy",
				"agent_id", st.agentID, "stream_id", stream, "err", err)
			m.SetDaemonStatus(stream, "unavailable")
			if m.RestartPolicy != nil {
				m.scheduleRestart(st.agentID, stream, st.params, "stale-recycle-failed")
			}
		}
	}()
	return true
}

// awaitDaemonBoot waits for an in-flight boot of streamID (a stale recycle or
// a concurrent SpawnDaemon) so a turn is not refused in that window.
func (m *AgentManager) awaitDaemonBoot(ctx context.Context, streamID string) {
	m.daemons.mu.Lock()
	ch, ok := m.daemons.booting[streamID]
	m.daemons.mu.Unlock()
	if !ok {
		return
	}
	select {
	case <-ch:
	case <-ctx.Done():
	}
}
