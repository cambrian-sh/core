package agentmgr

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// Heartbeat liveness for long-lived agent processes (ADR-0070 D3, ENG-1348).
//
// The crash watcher sees only an EXIT. A daemon or pool worker that stays alive but
// serves nobody — a chat worker stuck in Execute after the kernel gave up on the turn,
// a wedged interpreter — was reported running forever, and with a pool of one the
// whole chat lane stayed down until the hang ended by itself. The SDK (0.1.8+) now
// beats on its SignalStream every interval while it can serve (it withholds the beat
// while a call its caller abandoned is still running; see the SDK's liveness.py); the
// kernel records the last beat per INSTANCE and, after N missed beats, kills the
// process and hands the stream to the restart policy — the same path a crash takes.
//
// Tracking ARMS on an instance's first beat. An agent on an older SDK never beats, so
// it is never tracked and never killed for not sending what it cannot send: the
// behaviour is the capability, no SDK version is declared or parsed (chosen over a
// version/capability handshake, which the kernel has no channel for).

// beatState is one tracked instance.
type beatState struct {
	streamID string
	last     time.Time
	// busy is the latest beat's claim that a call is in flight; idleAt is the time of
	// the most recent beat that said it was not.
	busy   bool
	idleAt time.Time
}

// heartbeatTracker is the per-instance last-beat table and its watcher goroutine's
// lifecycle. The zero value is disabled (interval 0).
type heartbeatTracker struct {
	mu         sync.Mutex
	interval   time.Duration
	missed     int
	byInstance map[string]*beatState
	now        func() time.Time

	// watcher lifecycle (AGENTS.md §3.3: one owner per background goroutine).
	stop chan struct{}
	done chan struct{}
}

// ConfigureHeartbeat sets the beat interval and the missed beats before a kill
// (execution.agents.daemon_heartbeat_interval_ms / daemon_heartbeat_missed_before_kill)
// and passes the interval to every long-lived launch. interval <= 0 turns liveness
// off: no flag is passed, beats that arrive anyway are ignored, nothing is killed.
// Call before the first spawn and before StartHeartbeatWatch.
func (m *AgentManager) ConfigureHeartbeat(interval time.Duration, missed int) {
	if missed <= 0 {
		missed = 3
	}
	if interval < 0 {
		interval = 0
	}
	m.heartbeats.mu.Lock()
	m.heartbeats.interval = interval
	m.heartbeats.missed = missed
	m.heartbeats.mu.Unlock()
	m.InstanceManager.heartbeatInterval = interval
}

// HeartbeatInterval reports the configured interval (0 = off).
func (m *AgentManager) HeartbeatInterval() time.Duration {
	m.heartbeats.mu.Lock()
	defer m.heartbeats.mu.Unlock()
	return m.heartbeats.interval
}

func (h *heartbeatTracker) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

// RecordDaemonHeartbeat records one beat and reports whether it was attributed to a
// live daemon instance.
//
// instanceID is the instance the agent-plane verifier stamped from the caller's
// per-instance token (domain.AgentInstanceFromContext) — a kernel-held fact. Without
// one (an unauthenticated plane), the beat is attributed by streamID only when that
// stream's daemon belongs to agentID, the authenticated (or declared) sender. A beat
// for an instance that is not a registered daemon is dropped: liveness covers
// long-lived processes only.
func (m *AgentManager) RecordDaemonHeartbeat(instanceID, streamID, agentID string, busy bool) bool {
	if m.HeartbeatInterval() <= 0 {
		return false
	}
	stream, inst := m.resolveBeat(instanceID, streamID, agentID)
	if inst == "" {
		return false
	}
	h := &m.heartbeats
	now := h.clock()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.byInstance == nil {
		h.byInstance = make(map[string]*beatState)
	}
	st, ok := h.byInstance[inst]
	if !ok {
		st = &beatState{streamID: stream}
		h.byInstance[inst] = st
		slog.Info("ADR-0070 D3: heartbeat tracking armed", "stream_id", stream, "instance_id", inst)
	}
	st.last = now
	st.busy = busy
	if !busy {
		st.idleAt = now
	}
	return true
}

// resolveBeat maps a beat onto (stream, instance) of a registered daemon.
func (m *AgentManager) resolveBeat(instanceID, streamID, agentID string) (string, string) {
	m.daemons.mu.Lock()
	defer m.daemons.mu.Unlock()
	if instanceID != "" {
		for stream, s := range m.daemons.byStream {
			if s.instanceID == instanceID {
				return stream, instanceID
			}
		}
		return "", ""
	}
	if streamID == "" || agentID == "" {
		return "", ""
	}
	if s, ok := m.daemons.byStream[streamID]; ok && s.instanceID != "" && s.agentID == agentID {
		return streamID, s.instanceID
	}
	return "", ""
}

// DaemonServing reports, for the daemon currently registered on streamID, whether it
// is heartbeat-tracked and whether it has reported itself idle (a beat with no call in
// flight) after since. The agent pool uses it to hold a worker whose turn it abandoned
// out of rotation until the worker can serve again (agentpool.Dispatch). An untracked
// worker (older SDK, liveness off) reports tracked=false: the caller cannot know, and
// falls back to its old behaviour.
func (m *AgentManager) DaemonServing(streamID string, since time.Time) (tracked, idle bool) {
	instanceID, ok := m.DaemonInstanceID(streamID)
	if !ok {
		return false, false
	}
	h := &m.heartbeats
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.byInstance[instanceID]
	if !ok {
		return false, false
	}
	return true, st.idleAt.After(since)
}

// forgetHeartbeat drops an instance's tracking (it exited or was stopped).
func (m *AgentManager) forgetHeartbeat(instanceID string) {
	h := &m.heartbeats
	h.mu.Lock()
	delete(h.byInstance, instanceID)
	h.mu.Unlock()
}

// StartHeartbeatWatch starts the goroutine that reclaims instances past their missed-
// beat budget. A no-op when liveness is off or the watch already runs. Stop with
// StopHeartbeatWatch.
func (m *AgentManager) StartHeartbeatWatch(ctx context.Context) {
	h := &m.heartbeats
	h.mu.Lock()
	if h.interval <= 0 || h.stop != nil {
		h.mu.Unlock()
		return
	}
	tick := h.interval / 2
	h.stop = make(chan struct{})
	h.done = make(chan struct{})
	stop, done := h.stop, h.done
	h.mu.Unlock()

	go func() {
		defer close(done)
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-t.C:
				m.reclaimSilentDaemons()
			}
		}
	}()
}

// StopHeartbeatWatch stops the watcher and waits for it. Idempotent.
func (m *AgentManager) StopHeartbeatWatch() {
	h := &m.heartbeats
	h.mu.Lock()
	stop, done := h.stop, h.done
	h.stop, h.done = nil, nil
	h.mu.Unlock()
	if stop == nil {
		return
	}
	close(stop)
	<-done
}

// reclaimSilentDaemons kills every tracked instance whose last beat is older than
// missed × interval. Exported to tests through the watcher only.
func (m *AgentManager) reclaimSilentDaemons() {
	h := &m.heartbeats
	now := h.clock()
	type victim struct {
		instanceID, streamID string
		silent               time.Duration
	}
	var victims []victim
	h.mu.Lock()
	budget := time.Duration(h.missed) * h.interval
	for inst, st := range h.byInstance {
		if silent := now.Sub(st.last); silent > budget {
			victims = append(victims, victim{inst, st.streamID, silent})
			delete(h.byInstance, inst)
		}
	}
	h.mu.Unlock()
	for _, v := range victims {
		m.reclaimHungDaemon(v.streamID, v.instanceID, v.silent)
	}
}

// reclaimHungDaemon kills a daemon instance that stopped beating and hands its stream
// to the restart policy, exactly as a crash would (ADR-0070 D1/D2): restart with
// backoff, quarantine on a loop.
//
// The registry entry is removed BEFORE the kill, so a caller whose call fails when the
// process dies already sees the worker as not live (the pool then reports
// ErrWorkerLost and respawns instead of returning a raw transport error). The exit is
// marked expected so the crash watcher does not schedule a second restart.
func (m *AgentManager) reclaimHungDaemon(streamID, instanceID string, silent time.Duration) {
	m.daemons.mu.Lock()
	s, ok := m.daemons.byStream[streamID]
	if !ok || s.instanceID != instanceID {
		// Already stopped, crashed or replaced — nothing of ours to kill.
		m.daemons.mu.Unlock()
		return
	}
	agentID, params := s.agentID, s.params
	delete(m.daemons.byStream, streamID)
	m.daemons.mu.Unlock()

	slog.Warn("ADR-0070 D3: daemon stopped heartbeating; killing it for restart",
		"agent_id", agentID, "stream_id", streamID, "instance_id", instanceID, "silent_for", silent)
	m.markExpectedStop(instanceID)
	m.EvictInstance(instanceID)
	m.SetDaemonStatus(streamID, "unavailable")
	m.publishEvent(domain.DaemonCrashedEvent{AgentID: agentID, StreamID: streamID})

	if m.RestartPolicy == nil {
		return
	}
	m.scheduleRestart(agentID, streamID, params, "heartbeat-missed")
}
