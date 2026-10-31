package config

import (
	"reflect"
	"strings"
	"testing"
)

// Retiring a config key must not break a deployment that still sets it: the config
// schema is a held-stable contract (root AGENTS.md §3.4, ADR-0057 D8). These tests
// pin that for the keys retired 2026-10-31 with the ADR-0029 episodic lane —
// `consolidation_threshold_doc_count`, `consolidation_threshold_bytes` and the
// "episodic" HippocampusPolicy — through every layer an operator can set them in:
// a v1 flat file, a v2 nested file, the ADR-0101 store and a CAMBRIAN_* variable.

func TestRetiredEpisodicKeys_StillLoad(t *testing.T) {
	cases := map[string]string{
		"v1 flat": `{"execution": {
			"consolidation_threshold_doc_count": 5000,
			"consolidation_threshold_bytes": 1073741824,
			"ewma_alpha": 0.42
		}}`,
		"v2 nested": `{"schema_version": 2, "execution": {
			"memory": {"consolidation_threshold_doc_count": 5000, "consolidation_threshold_bytes": 1073741824},
			"supervision": {"ewma_alpha": 0.42}
		}}`,
	}
	for name, tuning := range cases {
		t.Run(name, func(t *testing.T) {
			dir := baseBundle(t)
			writeCfg(t, dir, "tuning.json", tuning)
			t.Setenv("CAMBRIAN_EXECUTION__CONSOLIDATION_THRESHOLD_DOC_COUNT", "7")
			t.Setenv("CAMBRIAN_EXECUTION__MEMORY__CONSOLIDATION_THRESHOLD_BYTES", "9")
			store := mapStore{
				"execution.consolidation_threshold_doc_count":         float64(11),
				"execution.memory.consolidation_threshold_bytes":      float64(13),
				"execution.hippocampus.hippocampus_policies.episodic": map[string]any{"similarity": 0.65},
			}

			cfg, _ := loadFrom(t, dir, store)

			// The file was honoured around the retired keys: they do not poison
			// the layer they sit in.
			if got := cfg.Execution.Supervision.EWMAAlpha; got != 0.42 {
				t.Errorf("ewma_alpha from the same layer = %v, want 0.42", got)
			}
			if err := cfg.Execution.Validate(); err != nil {
				t.Errorf("Validate rejected a config carrying retired keys: %v", err)
			}
		})
	}
}

// An operator file that still declares the "episodic" policy — and even names it
// the default — keeps loading and validating: the policy map accepts any name.
func TestRetiredEpisodicPolicy_OperatorDeclaredStillValid(t *testing.T) {
	dir := baseBundle(t)
	writeCfg(t, dir, "tuning.json", `{"execution": {"hippocampus": {
		"hippocampus_policies": {"episodic": {"similarity": 0.65, "confidence": 0, "max_age_hours": 8760}},
		"hippocampus_default_policy": "episodic"
	}}}`)
	cfg, _ := loadFrom(t, dir, nil)
	if err := cfg.Execution.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if _, ok := cfg.Execution.Hippocampus.HippocampusPolicies["episodic"]; !ok {
		t.Error("operator-declared episodic policy was dropped")
	}
}

// The retired keys must be gone from the schema, not merely unused: a field that
// comes back would be a declared-but-unread knob again (AGENTS.md §3.4).
func TestRetiredEpisodicKeys_AbsentFromSchema(t *testing.T) {
	retired := map[string]bool{
		"consolidation_threshold_doc_count": true,
		"consolidation_threshold_bytes":     true,
	}
	var walk func(ty reflect.Type, path string)
	walk = func(ty reflect.Type, path string) {
		for i := range ty.NumField() {
			f := ty.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if retired[name] {
				t.Errorf("retired key %s.%s is back in ExecutionConfig", path, name)
			}
			if f.Type.Kind() == reflect.Struct {
				walk(f.Type, path+"."+name)
			}
		}
	}
	walk(reflect.TypeOf(ExecutionConfig{}), "execution")
	for k := range retired {
		if _, ok := legacyExecutionKey[k]; ok {
			t.Errorf("retired key %q still has a v1→v2 migration route", k)
		}
	}
	if _, ok := DefaultConfig().Execution.Hippocampus.HippocampusPolicies["episodic"]; ok {
		t.Error(`DefaultConfig still ships the retired "episodic" HippocampusPolicy`)
	}
}

// retiredKeys2026_10_31 is every key the 2026-10-31 dead-key sweep deleted from
// the schema (owner approved deleting rather than wiring), with the ticket that
// carries the evidence that nothing read it. Paths are the v2 nested spelling.
var retiredKeys2026_10_31 = map[string]string{
	"execution.capability.capability_cluster_threshold":        "T-20261031-dead-capability-cluster-config-keys-and",
	"execution.capability.capability_cluster_epsilon":          "T-20261031-dead-capability-cluster-config-keys-and",
	"execution.capability.capability_cluster_min_agents":       "T-20261031-dead-capability-cluster-config-keys-and",
	"execution.capability.capability_cluster_interval_seconds": "T-20261031-dead-capability-cluster-config-keys-and",
	"execution.gatekeeper.min_auction_confidence":              "T-20261031-min-auction-confidence-config-is-dead-after-the",
	"execution.graph.weight_contradicts":                       "T-20261031-graph-edge-weight-and-consolidator-timeout",
	"execution.graph.weight_specifies":                         "T-20261031-graph-edge-weight-and-consolidator-timeout",
	"execution.graph.weight_closes":                            "T-20261031-graph-edge-weight-and-consolidator-timeout",
	"execution.graph.weight_discussed_in":                      "T-20261031-graph-edge-weight-and-consolidator-timeout",
	"execution.graph.consolidator_llm_timeout":                 "T-20261031-graph-edge-weight-and-consolidator-timeout",
	"execution.router.k_anonymity_floor":                       "T-20261031-three-config-fields-are-parsed-and-defaulted",
	"execution.ingestion.inbox_dir":                            "T-20261031-three-config-fields-are-parsed-and-defaulted",
	"embedder.supports_long_context":                           "T-20261031-three-config-fields-are-parsed-and-defaulted",
	"agent_pool.default_agent_timeout_ms":                      "T-20261031-agent-pool-default-agent-timeout-ms-is",
	"execution.memory.max_orphaned_documents":                  "T-20261031-execution-memory-max-orphaned-documents-and",
}

// TestRemovedExecutionKeysAreIgnored is the operator-facing guarantee for the
// 2026-10-31 sweep: a v2 file, a CAMBRIAN_* variable and an ADR-0101 store row
// that still set a deleted key all load without error, and a live sibling in the
// same layer is still honoured. The decoder has no ErrorUnused, so an unknown
// key is tolerated by construction; this test pins that for each key by name so
// the tolerance cannot be lost to a future decoder-strictness change unnoticed.
func TestRemovedExecutionKeysAreIgnored(t *testing.T) {
	dir := baseBundle(t)
	writeCfg(t, dir, "tuning.json", `{"schema_version": 2,
		"execution": {
			"capability": {"capability_cluster_threshold": 0.8, "capability_cluster_epsilon": 0.02,
				"capability_cluster_min_agents": 3, "capability_cluster_interval_seconds": 3600,
				"canonical_vocab": true},
			"gatekeeper": {"min_auction_confidence": 0.3, "gatekeeper_w1": 0.41},
			"graph": {"weight_contradicts": 0.9, "weight_specifies": 0.7, "weight_closes": 0.6,
				"weight_discussed_in": 0.5, "consolidator_llm_timeout": 60, "max_depth": 4},
			"router": {"k_anonymity_floor": 5},
			"ingestion": {"inbox_dir": "data/inbox", "ingestion_workers": 7},
			"memory": {"max_orphaned_documents": 100}
		},
		"embedder": {"supports_long_context": true},
		"agent_pool": {"default_agent_timeout_ms": 30000}
	}`)
	t.Setenv("CAMBRIAN_AGENT_POOL__DEFAULT_AGENT_TIMEOUT_MS", "5000")
	t.Setenv("CAMBRIAN_EXECUTION__GRAPH__WEIGHT_CLOSES", "0.1")
	t.Setenv("CAMBRIAN_EXECUTION__MIN_AUCTION_CONFIDENCE", "0.9") // v1 flat env spelling
	store := mapStore{
		"execution.capability.capability_cluster_threshold": 0.5,
		"execution.inbox_dir":                               "elsewhere", // v1 flat store spelling
	}

	cfg, _ := loadFrom(t, dir, store)

	if got := cfg.Execution.Capability.CanonicalVocab; !got {
		t.Error("canonical_vocab from the same layer as a retired key was not honoured")
	}
	if got := cfg.Execution.Gatekeeper.GatekeeperW1; got != 0.41 {
		t.Errorf("gatekeeper_w1 from the same layer = %v, want 0.41", got)
	}
	if got := cfg.Execution.Graph.MaxDepth; got != 4 {
		t.Errorf("graph.max_depth from the same layer = %v, want 4", got)
	}
	if got := cfg.Execution.Ingestion.IngestionWorkers; got != 7 {
		t.Errorf("ingestion_workers from the same layer = %v, want 7", got)
	}
	if err := cfg.Execution.Validate(); err != nil {
		t.Errorf("Validate rejected a config carrying retired keys: %v", err)
	}
}

// The 2026-10-31 keys must be gone from the schema, not merely unused, and must
// have no v1→v2 migration route (a route would re-plant them in a block).
func TestRemovedExecutionKeys_AbsentFromSchema(t *testing.T) {
	leaves := map[string]bool{}
	for path := range retiredKeys2026_10_31 {
		leaves[path[strings.LastIndex(path, ".")+1:]] = true
	}
	var walk func(ty reflect.Type, path string)
	walk = func(ty reflect.Type, path string) {
		for i := range ty.NumField() {
			f := ty.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if leaves[name] {
				t.Errorf("retired key %s.%s is back in the schema", path, name)
			}
			if f.Type.Kind() == reflect.Struct {
				walk(f.Type, path+"."+name)
			}
		}
	}
	walk(reflect.TypeOf(Config{}), "")
	for k := range leaves {
		if _, ok := legacyExecutionKey[k]; ok {
			t.Errorf("retired key %q still has a v1→v2 migration route", k)
		}
	}
}
