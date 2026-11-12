package config

import (
	stdjson "encoding/json"
	"strings"
	"testing"
)

// The content-size limits moved into execution.content on 2026-11-12 (owner
// decision, T-20261112-worker-resolve-cid-inline-has-no-size-cap-a /
// T-20261112-summarise-a-document-step-gets-only-the-first). An operator's
// explicit old key still holds, through every layer it can be set in.

func TestRelocatedContentKeys_LoadAtTheirNewPlace(t *testing.T) {
	cases := map[string]struct {
		tuning     string
		env        map[string]string
		store      mapStore
		wantPage   int
		wantDepMax int
	}{
		"v2 file, old keys": {
			tuning:   `{"schema_version": 2, "execution": {"ledger": {"page_bytes": 8000}, "plan": {"dependency_results_max_chars": 30000}}}`,
			wantPage: 8000, wantDepMax: 30000,
		},
		"store, old keys": {
			store:    mapStore{"execution.ledger.page_bytes": float64(9000), "execution.plan.dependency_results_max_chars": float64(31000)},
			wantPage: 9000, wantDepMax: 31000,
		},
		"env, old names": {
			env:      map[string]string{"CAMBRIAN_EXECUTION__LEDGER__PAGE_BYTES": "7000", "CAMBRIAN_EXECUTION__PLAN__DEPENDENCY_RESULTS_MAX_CHARS": "32000"},
			wantPage: 7000, wantDepMax: 32000,
		},
		// The new place stated in the same layer is the newer intent and wins.
		"both in one file": {
			tuning:   `{"execution": {"ledger": {"page_bytes": 8000}, "content": {"page_bytes": 12000}}}`,
			wantPage: 12000,
		},
		"both env vars": {
			env:      map[string]string{"CAMBRIAN_EXECUTION__LEDGER__PAGE_BYTES": "7000", "CAMBRIAN_EXECUTION__CONTENT__PAGE_BYTES": "11000"},
			wantPage: 11000,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := baseBundle(t)
			if tc.tuning != "" {
				writeCfg(t, dir, "tuning.json", tc.tuning)
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			var store Store
			if tc.store != nil {
				store = tc.store
			}
			cfg, _ := loadFrom(t, dir, store)
			c := cfg.Execution.Content
			if got := c.Page(); got != tc.wantPage {
				t.Errorf("content.page_bytes = %d, want %d", got, tc.wantPage)
			}
			if got := c.DependencyMaxBytes; got != tc.wantDepMax {
				t.Errorf("content.dependency_max_bytes = %d, want %d", got, tc.wantDepMax)
			}
			if err := cfg.Execution.Validate(); err != nil {
				t.Errorf("Validate: %v", err)
			}
		})
	}
}

func TestRelocatedContentKeys_MigrationIsIdempotent(t *testing.T) {
	in := []byte(`{"execution": {"plan": {"dependency_results_max_chars": 24000, "max_replan_attempts": 2}, "ledger": {"enabled": true, "page_bytes": 16000}}}`)
	once := migrateJSON(in, "test")
	twice := migrateJSON(once, "test")
	if string(once) != string(twice) {
		t.Fatalf("not idempotent:\n once=%s\ntwice=%s", once, twice)
	}
	var doc map[string]any
	if err := stdjson.Unmarshal(once, &doc); err != nil {
		t.Fatal(err)
	}
	exec := doc["execution"].(map[string]any)
	if _, left := exec["plan"].(map[string]any)["dependency_results_max_chars"]; left {
		t.Error("dependency_results_max_chars left in plan")
	}
	if _, left := exec["ledger"].(map[string]any)["page_bytes"]; left {
		t.Error("page_bytes left in ledger")
	}
	content := exec["content"].(map[string]any)
	if content["dependency_max_bytes"] != 24000.0 || content["page_bytes"] != 16000.0 {
		t.Errorf("content = %v", content)
	}
}

// Defaults: a 37,444-byte upstream report (p12) fits the receiver-sized budget,
// where the old fixed 24000-byte budget cut it.
func TestContentDefaults_DependencyBudgetFromReceiver(t *testing.T) {
	cfg, _ := loadFrom(t, baseBundle(t), nil)
	c := cfg.Execution.Content
	if c.Inline() != 65536 || c.Page() != 16000 || c.Preview() != 600 || c.ToolOutputMax() != 24000 || c.WorkerPromptBudget() != 32000 {
		t.Fatalf("defaults = inline %d page %d preview %d tool_output_max %d budget %d",
			c.Inline(), c.Page(), c.Preview(), c.ToolOutputMax(), c.WorkerPromptBudget())
	}
	if got := c.DependencyBudgetTokens(); got != 16000 {
		t.Errorf("dependency budget = %d tokens, want 16000 (half the worker's 32000)", got)
	}
	if got := c.DependencyBudgetBytes(); got < 37444 {
		t.Errorf("dependency budget = %d bytes; p12's 37,444-byte report must arrive whole", got)
	}
	if got := (ContentConfig{DependencyMaxBytes: 24000}).DependencyBudgetBytes(); got != 24000 {
		t.Errorf("an explicit byte ceiling must cap the budget: got %d", got)
	}
}

func TestContentValidate(t *testing.T) {
	for name, c := range map[string]ContentConfig{
		"negative":                 {InlineResultBytes: -1},
		"fraction over 1":          {DependencyBudgetFraction: 1.5},
		"page over tool cap":       {PageBytes: 30000},
		"preview not under a page": {PreviewChars: 16000},
	} {
		if errs := c.validate(); len(errs) == 0 {
			t.Errorf("%s: no validation error", name)
		}
	}
	if errs := (ContentConfig{}).validate(); len(errs) != 0 {
		t.Errorf("zero config: %s", strings.Join(errs, "; "))
	}
}
