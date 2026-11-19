package app

import (
	"testing"

	"github.com/cambrian-sh/core/internal/config"
)

// ENG-531: execution.tools.rule_of_two reaches the executor control — the
// default config builds a gate, enabled=false builds none, and an inert policy
// is refused at boot rather than silently wired as off.
func TestRuleOfTwoGate_BuiltFromConfig(t *testing.T) {
	cfg := config.DefaultConfig().Execution.Tools.RuleOfTwo
	g, err := ruleOfTwoGate(cfg)
	if err != nil || g == nil {
		t.Fatalf("default config must build the gate: %v", err)
	}
	// ENG-2247: the default kernel_write_effects reach the gate — every write
	// verb the agents use is decided as a write.
	for _, verb := range []string{"propose_memory_note", "schedule_task", "manage_scheduled_task", "propose_skill",
		"propose_knowledge", "propose_ontology_change", "forget", "ingest_memory"} {
		if tool := g.KernelWriteTool(verb); len(tool.Effects) != 1 || tool.Effects[0] != "write" {
			t.Errorf("%s: effects %v, want [write] from the default config", verb, tool.Effects)
		}
	}
	cfg.Enabled = false
	if g, err := ruleOfTwoGate(cfg); err != nil || g != nil {
		t.Fatalf("enabled=false must build no gate: %v %v", g, err)
	}
	cfg.Enabled, cfg.UntrustedSources = true, nil
	if _, err := ruleOfTwoGate(cfg); err == nil {
		t.Fatal("an empty untrusted_sources must be refused")
	}
}
