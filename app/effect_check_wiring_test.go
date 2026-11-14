package app

import (
	"context"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/awareness"
	"github.com/cambrian-sh/core/internal/config"
)

type wiringAgents struct{}

func (wiringAgents) GetAllAgents(context.Context) ([]domain.AgentDefinition, error) {
	return []domain.AgentDefinition{{ID: "assistant"}}, nil
}
func (wiringAgents) GetManifest(context.Context, string) (*domain.AgentManifest, error) {
	return &domain.AgentManifest{Capabilities: []string{"general_purpose"}, ToolAllowlist: []string{"mcp:erp/*", "mcp:filesystem/*"}}, nil
}

type wiringTools map[string][]domain.SystemTool

func (w wiringTools) AvailableTools(_ context.Context, id string) []domain.SystemTool { return w[id] }

// wireEffectCheck gives the planner the executor's menu as well as the effect
// source, so the planner plans from the tools an agent can call, not from its
// declared globs (T-20261107-world-system-asks-are-planned-as-local-file).
func TestWireEffectCheckGivesThePlannerTheLiveToolMenu(t *testing.T) {
	planner := awareness.NewPlanner(nil, wiringAgents{})
	planner.SetCapabilityContract(true)
	tools := wiringTools{"assistant": {{Name: "mcp:erp/finance_aging_report", Description: "AR aging by customer.", Effects: []domain.ToolEffect{domain.EffectRead}}}}
	wireEffectCheck(config.DefaultConfig().Execution.Capability, tools, nil, planner)

	block, _, _, on := planner.CapabilityContract(context.Background())
	if !on {
		t.Fatal("capability contract off")
	}
	for _, want := range []string{"- assistant can call: erp (finance_aging_report)", "- finance_aging_report: AR aging by customer."} {
		if !strings.Contains(block, want) {
			t.Errorf("block lacks %q:\n%s", want, block)
		}
	}
	if strings.Contains(block, "filesystem") {
		t.Errorf("an unconnected server's glob reached the planner:\n%s", block)
	}
}
