package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// T-20261118-kg-extractor-graph-tier-cambrian-kg-graph-tier:
// execution.ingestion.kg_graph_tier defaults to "svo" (the kg_extractor's
// previous hard default), reaches the field from a file and from its env var,
// and anything but svo|ner is refused at load.
func TestKgGraphTier_DefaultFileEnvAndValidation(t *testing.T) {
	cfg := DefaultConfig()
	if got := cfg.Execution.Ingestion.KgGraphTier; got != KgGraphTierSVO {
		t.Fatalf("default = %q, want %q", got, KgGraphTierSVO)
	}
	if err := cfg.Execution.Validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
	cfg.Execution.Ingestion.KgGraphTier = ""
	if err := cfg.Execution.Validate(); err != nil {
		t.Fatalf(`"" (reads as svo) must be valid: %v`, err)
	}

	dir := baseBundle(t)
	writeCfg(t, dir, "tuning.json", `{"schema_version":2,"execution":{"ingestion":{"kg_graph_tier":"ner"}}}`)
	got, _ := loadFrom(t, dir, nil)
	if got.Execution.Ingestion.KgGraphTier != KgGraphTierNER {
		t.Fatalf("file key did not reach the field: %q", got.Execution.Ingestion.KgGraphTier)
	}

	dir = baseBundle(t)
	writeCfg(t, dir, "tuning.json", `{"schema_version":2,"execution":{"ingestion":{"kg_graph_tier":"llm"}}}`)
	if _, _, err := LoadConfigWithStore(filepath.Join(dir, "config.json"), nil); err == nil ||
		!strings.Contains(err.Error(), "kg_graph_tier") {
		t.Fatalf("an unknown tier was not refused at load: %v", err)
	}

	dir = baseBundle(t)
	t.Setenv("CAMBRIAN_EXECUTION__INGESTION__KG_GRAPH_TIER", "ner")
	got, _ = loadFrom(t, dir, nil)
	if got.Execution.Ingestion.KgGraphTier != KgGraphTierNER {
		t.Fatalf("CAMBRIAN_EXECUTION__INGESTION__KG_GRAPH_TIER did not reach the field: %q", got.Execution.Ingestion.KgGraphTier)
	}
}
