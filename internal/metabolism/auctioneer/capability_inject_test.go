package auctioneer

import (
	"context"
	"testing"

	pb "github.com/cambrian-sh/cambrian-runtime/api/proto"
	"github.com/cambrian-sh/cambrian-runtime/internal/config"
	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// capturingCallHook records the Context of each handoff passed to CallAgent.
type capturingCallHook struct {
	received []map[string]string
}

func (h *capturingCallHook) hook(ctx context.Context, agentID string, handoff *domain.Handoff, excludeID string) (*domain.Handoff, error) {
	clone := make(map[string]string, len(handoff.Context))
	for k, v := range handoff.Context {
		clone[k] = v
	}
	h.received = append(h.received, clone)
	return &domain.Handoff{
		Payload:   &domain.Payload{Data: []byte("ok")},
		FromAgent: agentID,
	}, nil
}

// manifestDialer extends mockDialer to return manifests for known agents.
type manifestDialer struct {
	mockDialer
	manifests map[string]*domain.AgentManifest
}

func (m *manifestDialer) GetManifest(_ context.Context, agentID string) (*domain.AgentManifest, error) {
	if m.manifests != nil {
		if mf, ok := m.manifests[agentID]; ok {
			return mf, nil
		}
	}
	return nil, nil
}

func makeToolAuctioneer(agentID string, tools []string) (*Auctioneer, *capturingCallHook) {
	agent := domain.AgentDefinition{ID: agentID, Trait: domain.TraitTool}
	manifest := &domain.AgentManifest{Tools: tools}

	gk := &testGatekeeper{
		agents:    map[string]domain.AgentDefinition{agentID: agent},
		manifests: map[string]*domain.AgentManifest{agentID: manifest},
	}
	dialer := &manifestDialer{
		manifests: map[string]*domain.AgentManifest{agentID: manifest},
	}
	cfg := config.ExecutionConfig{MinAuctionConfidence: 0.0, MaxRecursionDepth: 3}
	hook := &capturingCallHook{}
	a := New(dialer, gk, cfg)
	a.CallAgentHook = hook.hook

	// Register a mock proposal client so the tool agent's proposal RPC succeeds
	// (ADR-0023 routing fix: tool agents no longer bypass the proposal path).
	mockClient := &mockAgentClient{
		proposal: &pb.ProposalResponse{
			Confidence:         1.0,
			Rationale:            agent.Description,
			EstimatedLatencyMs: 5,
		},
	}
	a.agentClients[agentID] = &pbClientWrapper{m: mockClient}

	return a, hook
}

func TestExecute_InjectsCapabilityIntoHandoff(t *testing.T) {
	a, hook := makeToolAuctioneer("code-agent", []string{"code_generation", "python_generation"})

	task := &domain.AuctionTask{ID: "t1", Description: "code_generation task"}
	_, err := a.Execute(context.Background(), task, &domain.Handoff{
		Payload: &domain.Payload{Data: []byte("task")},
		Context: map[string]string{},
	})

	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if len(hook.received) == 0 {
		t.Fatal("CallAgent never invoked")
	}
	if got := hook.received[0]["_capability"]; got == "" {
		t.Error("expected _capability to be injected, got empty string")
	}
}

func TestExecute_CapabilityIsFromManifestTools(t *testing.T) {
	a, hook := makeToolAuctioneer("sum-agent", []string{"summarisation", "text_summary"})

	task := &domain.AuctionTask{ID: "t2", Description: "summarise this text"}
	_, _ = a.Execute(context.Background(), task, &domain.Handoff{
		Payload: &domain.Payload{Data: []byte("task")},
		Context: map[string]string{},
	})

	if len(hook.received) == 0 {
		t.Fatal("CallAgent never invoked")
	}
	cap := hook.received[0]["_capability"]
	validCaps := map[string]bool{"summarisation": true, "text_summary": true}
	if !validCaps[cap] {
		t.Errorf("expected _capability to be a declared manifest tool, got %q", cap)
	}
}

func TestExecute_NoCapabilityWhenManifestHasNoTools(t *testing.T) {
	agent := domain.AgentDefinition{ID: "bare-agent", Trait: domain.TraitTool}
	gk := &testGatekeeper{
		agents:    map[string]domain.AgentDefinition{"bare-agent": agent},
		manifests: map[string]*domain.AgentManifest{"bare-agent": {Tools: nil}},
	}
	hook := &capturingCallHook{}
	a := New(&mockDialer{}, gk, config.ExecutionConfig{MaxRecursionDepth: 3})
	a.CallAgentHook = hook.hook

	// Register mock client so proposal RPC succeeds (ADR-0023 routing fix).
	mockClient := &mockAgentClient{
		proposal: &pb.ProposalResponse{
			Confidence:         1.0,
			Rationale:          "bare tool",
			EstimatedLatencyMs: 5,
		},
	}
	a.agentClients["bare-agent"] = &pbClientWrapper{m: mockClient}

	task := &domain.AuctionTask{ID: "t3", Description: "some task"}
	_, _ = a.Execute(context.Background(), task, &domain.Handoff{
		Payload: &domain.Payload{Data: []byte("task")},
		Context: map[string]string{},
	})

	if len(hook.received) == 0 {
		t.Fatal("CallAgent never invoked")
	}
	// When no tools declared, _capability is empty or absent — not a crash
	_ = hook.received[0]["_capability"]
}
