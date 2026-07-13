package watcher

import (
	"context"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/metabolism/agentmgr"
)

type mockAgentManager struct {
	instances map[string]*struct{ agentID, instanceID string }
}

func (m *mockAgentManager) FindInstanceByToken(_ string) *domain.Instance { return nil }
func (m *mockAgentManager) GetInstanceIDs(_ string) []string              { return nil }
func (m *mockAgentManager) EvictInstance(id string) {
	if _, ok := m.instances[id]; ok {
		delete(m.instances, id)
	}
}

func TestWatcher_CircuitBreaker_TracksInvalidSignals(t *testing.T) {
	m := &agentmgr.AgentManager{}
	w := New(m, nil, nil, WatcherConfig{
		SignalNoiseThreshold:  3,
		SignalNoiseWindowSecs: 10,
	})

	w.RecordInvalidSignal("agent-1")
	w.RecordInvalidSignal("agent-1")

	count := w.InvalidSignalCount("agent-1")
	if count != 2 {
		t.Errorf("expected 2 invalid signals, got %d", count)
	}
}

func TestWatcher_CircuitBreaker_ResetsOnValidSignal(t *testing.T) {
	m := &agentmgr.AgentManager{}
	w := New(m, nil, nil, WatcherConfig{
		SignalNoiseThreshold:  3,
		SignalNoiseWindowSecs: 10,
	})

	w.RecordInvalidSignal("agent-1")
	w.RecordInvalidSignal("agent-1")

	w.ResetInvalidSignals("agent-1")

	count := w.InvalidSignalCount("agent-1")
	if count != 0 {
		t.Errorf("expected 0 after reset, got %d", count)
	}
}

func TestWatcher_CircuitBreaker_InhibitsOnThreshold(t *testing.T) {
	mockMgr := &mockAgentManager{
		instances: map[string]*struct{ agentID, instanceID string }{},
	}

	w := New(mockMgr, nil, nil, WatcherConfig{
		SignalNoiseThreshold:  2,
		SignalNoiseWindowSecs: 10,
	})

	w.RecordInvalidSignal("agent-1")
	if w.ShouldInhibit("agent-1") {
		t.Error("should not inhibit after 1 invalid signal when threshold is 2")
	}

	w.RecordInvalidSignal("agent-1")
	if !w.ShouldInhibit("agent-1") {
		t.Error("should inhibit after 2 invalid signals")
	}
}

func TestWatcher_CircuitBreaker_SlidingWindow_OldSignalsDontCount(t *testing.T) {
	m := &agentmgr.AgentManager{}
	w := New(m, nil, nil, WatcherConfig{
		SignalNoiseThreshold:  3,
		SignalNoiseWindowSecs: 1,
	})

	w.recordSignalAt("agent-1", time.Now().Add(-2*time.Second))

	count := w.InvalidSignalCount("agent-1")
	if count != 0 {
		t.Errorf("old signals should be pruned from the window, got %d", count)
	}
}

func TestWatcher_HandleInvalidSignal_TriggersInhibit(t *testing.T) {
	mockMgr := &mockAgentManager{
		instances: map[string]*struct{ agentID, instanceID string }{
			"inst-1": {"agent-1", "inst-1"},
		},
	}

	w := New(mockMgr, nil, nil, WatcherConfig{
		SignalNoiseThreshold:  1,
		SignalNoiseWindowSecs: 10,
	})

	err := w.HandleInvalidSignal(context.Background(), &domain.Handoff{
		FromAgent: "agent-1",
		Context:   map[string]string{"_signal_type": "BROKEN"},
	})

	if err == nil {
		t.Error("expected an error when circuit breaker trips")
	}
}
