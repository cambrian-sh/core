package config

import (
	"strings"
	"testing"
)

// ADR-0136 D4/D5: execution.facts ships OFF, its keys reach the fields from env
// (CAMBRIAN_EXECUTION__FACTS__*, the names the adr0136-gate experiment spec uses), bad
// values are refused while their switch is on, and the retrieval fingerprint changes
// only when the lane (or lane + supersession) is on.
func TestFacts_DefaultsEnvValidationFingerprint(t *testing.T) {
	d := DefaultConfig().Execution
	f := d.Facts
	if f.ExtractEnabled || f.LaneEnabled || f.SupersessionEnabled || d.Ingestion.SyncIngestWaitForFacts {
		t.Fatalf("the fact layer must default off: %+v / wait %v", f, d.Ingestion.SyncIngestWaitForFacts)
	}
	if f.QueueSize != 1024 || f.Workers != 2 || f.LLMTimeoutMs != 120000 || f.MaxFactsPerDocument != 64 ||
		f.MaxPromptChars != 48000 || f.LaneTopK != 20 || f.FactMatchWeight != 0.2 || f.CurrentFactWeight != 0.1 ||
		f.FuzzyCandidates != 10 || len(f.SingleAttributes) != 0 || d.Ingestion.SyncIngestFactsWaitTimeoutMs != 120000 ||
		f.MaxCallsPerDocument != 8 || f.MaxFactsPerSource != 512 {
		t.Fatalf("tunable defaults: %+v", f)
	}

	dir := baseBundle(t)
	t.Setenv("CAMBRIAN_EXECUTION__FACTS__EXTRACT_ENABLED", "true")
	t.Setenv("CAMBRIAN_EXECUTION__FACTS__LANE_ENABLED", "true")
	t.Setenv("CAMBRIAN_EXECUTION__FACTS__SUPERSESSION_ENABLED", "true")
	t.Setenv("CAMBRIAN_EXECUTION__FACTS__LANE_TOP_K", "30")
	t.Setenv("CAMBRIAN_EXECUTION__FACTS__FACT_MATCH_WEIGHT", "0.3")
	t.Setenv("CAMBRIAN_EXECUTION__INGESTION__SYNC_INGEST_WAIT_FOR_FACTS", "true")
	// One attribute through env is weakly decoded into a one-element list; a longer
	// registry belongs in a config file (env values are not split).
	t.Setenv("CAMBRIAN_EXECUTION__FACTS__SINGLE_ATTRIBUTES", "home_city")
	// ADR-0136 A1 keys (the names experiments/adr0136-gate-facts.json uses).
	t.Setenv("CAMBRIAN_EXECUTION__FACTS__ASSESSMENTS_ENABLED", "true")
	t.Setenv("CAMBRIAN_EXECUTION__FACTS__CARDINALITY_MIN_VOTES", "4")
	t.Setenv("CAMBRIAN_EXECUTION__FACTS__MULTI_ATTRIBUTES", "likes")
	// ADR-0136 A2.3 keys.
	t.Setenv("CAMBRIAN_EXECUTION__FACTS__MAX_CALLS_PER_DOCUMENT", "0")
	t.Setenv("CAMBRIAN_EXECUTION__FACTS__MAX_FACTS_PER_SOURCE", "300")
	cfg, _ := loadFrom(t, dir, nil)
	g := cfg.Execution.Facts
	if !g.ExtractEnabled || !g.LaneEnabled || !g.SupersessionEnabled || g.LaneTopK != 30 || g.FactMatchWeight != 0.3 ||
		!cfg.Execution.Ingestion.SyncIngestWaitForFacts || len(g.SingleAttributes) != 1 || g.SingleAttributes[0] != "home_city" ||
		!g.AssessmentsEnabled || g.CardinalityMinVotes != 4 || len(g.MultiAttributes) != 1 || g.MultiAttributes[0] != "likes" ||
		g.MaxCallsPerDocument != 0 || g.MaxFactsPerSource != 300 {
		t.Fatalf("env did not reach the fields: %+v wait=%v", g, cfg.Execution.Ingestion.SyncIngestWaitForFacts)
	}

	for name, mut := range map[string]func(*ExecutionConfig){
		"facts.queue_size":             func(c *ExecutionConfig) { c.Facts.ExtractEnabled = true; c.Facts.QueueSize = 0 },
		"facts.workers":                func(c *ExecutionConfig) { c.Facts.ExtractEnabled = true; c.Facts.Workers = 0 },
		"facts.max_prompt_chars":       func(c *ExecutionConfig) { c.Facts.ExtractEnabled = true; c.Facts.MaxPromptChars = 10 },
		"facts.fuzzy_candidates":       func(c *ExecutionConfig) { c.Facts.ExtractEnabled = true; c.Facts.FuzzyCandidates = -1 },
		"facts.lane_top_k":             func(c *ExecutionConfig) { c.Facts.LaneEnabled = true; c.Facts.LaneTopK = 0 },
		"facts.fact_match_weight":      func(c *ExecutionConfig) { c.Facts.FactMatchWeight = -1 },
		"facts.single_attributes":      func(c *ExecutionConfig) { c.Facts.SingleAttributes = []string{" "} },
		"facts.multi_attributes":       func(c *ExecutionConfig) { c.Facts.MultiAttributes = []string{""} },
		"facts.cardinality_min_votes":  func(c *ExecutionConfig) { c.Facts.CardinalityMinVotes = -1 },
		"facts.max_calls_per_document": func(c *ExecutionConfig) { c.Facts.ExtractEnabled = true; c.Facts.MaxCallsPerDocument = -1 },
		"facts.max_facts_per_source":   func(c *ExecutionConfig) { c.Facts.ExtractEnabled = true; c.Facts.MaxFactsPerSource = 0 },
		"sync_ingest_facts_wait":       func(c *ExecutionConfig) { c.Ingestion.SyncIngestFactsWaitTimeoutMs = -1 },
	} {
		bad := DefaultConfig().Execution
		mut(&bad)
		if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: a bad value must be refused, got %v", name, err)
		}
	}
	if err := DefaultConfig().Execution.Validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}

	base := DefaultConfig().Execution
	same := DefaultConfig().Execution
	same.Facts.ExtractEnabled = true // the corpus changes, not the ranking function
	same.Facts.LaneTopK = 99         // read only when lane_enabled
	same.Facts.SupersessionEnabled = true
	if base.RetrievalFingerprint("e") != same.RetrievalFingerprint("e") {
		t.Fatal("extraction and lane-off tunables must not change the fingerprint")
	}
	lane := DefaultConfig().Execution
	lane.Facts.LaneEnabled = true
	sup := lane
	sup.Facts.SupersessionEnabled = true
	if lane.RetrievalFingerprint("e") == base.RetrievalFingerprint("e") || sup.RetrievalFingerprint("e") == lane.RetrievalFingerprint("e") {
		t.Fatal("the lane, and supersession on top of it, change ranking and the fingerprint")
	}
}
