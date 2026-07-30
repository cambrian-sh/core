package app

import (
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// The seam's whole value is that it holds without the plugin's cooperation.
func TestScopedAgentRegistrar_RefusesAnIDOutsideThePluginNamespace(t *testing.T) {
	var got []domain.AgentDefinition
	reg := scopedAgentRegistrar("telegram", func(d domain.AgentDefinition) error {
		got = append(got, d)
		return nil
	})

	if err := reg(domain.AgentDefinition{ID: "telegram_ingress_support"}); err != nil {
		t.Fatalf("own namespace must be allowed: %v", err)
	}
	// The id a plugin would choose to impersonate something else.
	if err := reg(domain.AgentDefinition{ID: "planner"}); err == nil {
		t.Fatal("a plugin must not be able to mint a principal outside its namespace")
	}
	if len(got) != 1 {
		t.Fatalf("only the namespaced definition should have reached the registry, got %+v", got)
	}
}

func TestScopedAgentRegistrar_ForcesAwayPrivilege(t *testing.T) {
	var got domain.AgentDefinition
	reg := scopedAgentRegistrar("telegram", func(d domain.AgentDefinition) error {
		got = d
		return nil
	})
	if err := reg(domain.AgentDefinition{ID: "telegram_x", System: true}); err != nil {
		t.Fatalf("register: %v", err)
	}
	// Forced rather than rejected: a plugin that asked and was refused simply
	// stops asking; one that gets an ordinary agent keeps working correctly.
	if got.System {
		t.Fatal("a plugin must not be able to grant itself system privilege")
	}
}

func TestScopedAgentRegistrar_NilWhenTheKernelHasNoRegistry(t *testing.T) {
	if scopedAgentRegistrar("telegram", nil) != nil {
		t.Fatal("a nil base must stay nil, so a plugin can tell it cannot register")
	}
}
