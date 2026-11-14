package agentplane

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// a2aDialer serves a RuntimeA2A agent through ExecuteA2A and counts boots.
type a2aDialer struct {
	countingDialer
	boots    int
	executed *domain.AgentDefinition
}

func (d *a2aDialer) GetAgentByName(_ context.Context, name string) (*domain.AgentDefinition, error) {
	return &domain.AgentDefinition{ID: name, Runtime: domain.RuntimeA2A}, nil
}
func (d *a2aDialer) GetOrBootInstance(ctx context.Context, def *domain.AgentDefinition, ex string) (*domain.Instance, error) {
	d.boots++
	return d.countingDialer.GetOrBootInstance(ctx, def, ex)
}
func (d *a2aDialer) ExecuteA2A(_ context.Context, def domain.AgentDefinition, _ *domain.Handoff) (*domain.Handoff, error) {
	d.executed = &def
	return &domain.Handoff{Payload: &domain.Payload{Data: []byte("remote answer")}}, nil
}

// A RuntimeA2A agent is served by the A2A client, never booted as a local
// process (whose boot path refuses the runtime), so it is reachable through the
// Transport like any other agent
// (T-20261113-agentmanager-callagent-kills-the-shared-jit).
func TestCallAgent_A2AAgentRoutesToA2AClient(t *testing.T) {
	d := &a2aDialer{}
	a := New(d)
	resp, err := a.CallAgent(context.Background(), "remote_agent", &domain.Handoff{}, "")
	if err != nil {
		t.Fatalf("CallAgent: %v", err)
	}
	if d.executed == nil || d.executed.ID != "remote_agent" {
		t.Fatalf("A2A client was not called (executed=%v)", d.executed)
	}
	if d.boots != 0 {
		t.Fatalf("an A2A agent was booted locally (%d boots)", d.boots)
	}
	if string(resp.Payload.Data) != "remote answer" {
		t.Fatalf("response = %q", resp.Payload.Data)
	}
}
