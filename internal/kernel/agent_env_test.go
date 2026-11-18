package kernel

import (
	"slices"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/internal/config"
)

// execution.ingestion.kg_graph_tier reaches spawned agents as
// CAMBRIAN_KG_GRAPH_TIER, the variable the kg_extractor reads; the default
// config sends "svo", and an empty value sends nothing (the agent's default).
func TestAgentEnvExtras_KgGraphTier(t *testing.T) {
	cfg := config.DefaultConfig()
	if got := AgentEnvExtras(cfg); !slices.Contains(got, "CAMBRIAN_KG_GRAPH_TIER=svo") {
		t.Fatalf("default extras lack the svo tier: %v", got)
	}
	cfg.Execution.Ingestion.KgGraphTier = config.KgGraphTierNER
	if got := AgentEnvExtras(cfg); !slices.Contains(got, "CAMBRIAN_KG_GRAPH_TIER=ner") {
		t.Fatalf("ner tier not delivered: %v", got)
	}
	cfg.Execution.Ingestion.KgGraphTier = ""
	for _, kv := range AgentEnvExtras(cfg) {
		if strings.HasPrefix(kv, "CAMBRIAN_KG_GRAPH_TIER=") {
			t.Fatalf("empty tier still set %s", kv)
		}
	}
}
