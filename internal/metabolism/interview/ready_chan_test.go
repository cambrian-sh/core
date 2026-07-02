package interview

import (
	"testing"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

func TestReadyChan_ReceivesAgentIDAfterInterview(t *testing.T) {
	agent := makeToolAgent("ready-agent", "hash-r")
	w := newWorker(newTestRegistry(agent), &mockEmbedder{results: [][]float32{{0.1}}},
		&mockProfileStore{}, &mockUpdater{})

	go func() { _ = w.processAgent(t.Context(), agent) }()

	select {
	case id := <-w.ReadyChan():
		if id != agent.ID {
			t.Errorf("expected %q, got %q", agent.ID, id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ready signal")
	}
}

func TestWaitForAgentReadiness_AllReady(t *testing.T) {
	agents := []string{"a1", "a2", "a3"}

	// pre-fill the channel to simulate agents that finished before we call wait
	w := &InterviewWorker{readyCh: make(chan string, 32)}
	for _, id := range agents {
		w.readyCh <- id
	}

	err := WaitForAgentReadiness(w, agents, 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWaitForAgentReadiness_Timeout(t *testing.T) {
	w := &InterviewWorker{readyCh: make(chan string, 32)}
	// Only send one of two required agents
	w.readyCh <- "agent-a"

	err := WaitForAgentReadiness(w, []string{"agent-a", "agent-b"}, 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

func TestWaitForAgentReadiness_ExtraSignalsIgnored(t *testing.T) {
	w := &InterviewWorker{readyCh: make(chan string, 32)}
	// Send target agents plus extra irrelevant IDs
	w.readyCh <- "wanted"
	w.readyCh <- "irrelevant"

	err := WaitForAgentReadiness(w, []string{"wanted"}, 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func makeToolAgent(id, hash string) domain.AgentDefinition {
	return domain.AgentDefinition{ID: id, SourceHash: hash, Trait: domain.TraitTool, Provisional: true}
}
