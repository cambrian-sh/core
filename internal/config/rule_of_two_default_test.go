package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/internal/config"
)

// ENG-531 (owner decision 2026-11-19): the Rule-of-Two gate is on by default,
// an operator can turn it off or narrow it in a file layer, and an inert or
// unknown policy is refused.
func TestRuleOfTwo_DefaultsOnOperatorCanNarrowAndInvalidIsRefused(t *testing.T) {
	d := config.DefaultConfig().Execution.Tools.RuleOfTwo
	if !d.Enabled || len(d.UntrustedSources) == 0 || len(d.GatedEffects) == 0 || len(d.SensitiveTags) != 0 {
		t.Fatalf("defaults: %+v", d)
	}
	load := func(t *testing.T, body string) (*config.Config, error) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return config.LoadConfig(path)
	}
	base := `"database":{"host":"localhost","port":"5432","user":"u","password":"p","dbname":"d"},"server":{"port":"50051"}`
	cfg, err := load(t, `{`+base+`}`)
	if err != nil || !cfg.Execution.Tools.RuleOfTwo.Enabled {
		t.Fatalf("a config that does not mention rule_of_two must load it on: %v", err)
	}
	cfg, err = load(t, `{"schema_version":4,`+base+`,"execution":{"tools":{"rule_of_two":{"enabled":true,"untrusted_sources":["web"],"sensitive_tags":["pii"]}}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if r := cfg.Execution.Tools.RuleOfTwo; len(r.UntrustedSources) != 1 || r.SensitiveTags[0] != "pii" || len(r.GatedEffects) == 0 {
		t.Errorf("narrowing not honoured (gated effects keep the default): %+v", r)
	}
	// ENG-2247: a file layer merges per verb over the default kernel_write_effects.
	cfg, err = load(t, `{"schema_version":4,`+base+`,"execution":{"tools":{"rule_of_two":{"kernel_write_effects":{"propose_skill":["read"]}}}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if k := cfg.Execution.Tools.RuleOfTwo.KernelWriteEffects; len(k["propose_skill"]) != 1 || k["propose_skill"][0] != "read" ||
		len(k["propose_memory_note"]) != 1 || k["propose_memory_note"][0] != "write" {
		t.Errorf("per-verb merge: %v", k)
	}
	cfg, err = load(t, `{"schema_version":4,`+base+`,"execution":{"tools":{"rule_of_two":{"enabled":false}}}}`)
	if err != nil || cfg.Execution.Tools.RuleOfTwo.Enabled {
		t.Errorf("explicit enabled=false not honoured: %v", err)
	}
	for name, mut := range map[string]func(*config.RuleOfTwoConfig){
		"unknown source": func(r *config.RuleOfTwoConfig) { r.UntrustedSources = []string{"email"} },
		"unknown effect": func(r *config.RuleOfTwoConfig) { r.GatedEffects = []string{"delete"} },
		"no sources":     func(r *config.RuleOfTwoConfig) { r.UntrustedSources = nil },
		"no effects":     func(r *config.RuleOfTwoConfig) { r.GatedEffects = nil },
		// ENG-2247: a kernel write verb must declare known effect classes.
		"verb unknown effect": func(r *config.RuleOfTwoConfig) {
			r.KernelWriteEffects = map[string][]string{"propose_memory_note": {"delete"}}
		},
		"verb no effect": func(r *config.RuleOfTwoConfig) {
			r.KernelWriteEffects = map[string][]string{"propose_memory_note": {}}
		},
	} {
		bad := config.DefaultConfig()
		mut(&bad.Execution.Tools.RuleOfTwo)
		if e := bad.Validate(); e == nil || !strings.Contains(e.Error(), "rule_of_two") {
			t.Errorf("%s must be refused, got %v", name, e)
		}
	}
}
