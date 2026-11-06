package config_test

import (
	"strings"
	"testing"

	"github.com/cambrian-sh/core/internal/config"
)

// ADR-0139 D6: execution.gatekeeper.merit_mode defaults to posterior_lcb (owner
// decision 2026-11-05; an unset mode reads the same) and its posterior parameters
// are validated at load.
func TestGatekeeperMeritMode_DefaultsAndValidation(t *testing.T) {
	def := config.DefaultConfig().Execution
	if def.Gatekeeper.ActiveMeritMode() != config.MeritModePosteriorLCB {
		t.Fatalf("default merit mode = %q, want posterior_lcb", def.Gatekeeper.ActiveMeritMode())
	}
	if (config.GatekeeperConfig{}).ActiveMeritMode() != config.MeritModePosteriorLCB {
		t.Fatalf("an unset merit mode must read as posterior_lcb")
	}
	if def.Gatekeeper.MeritPriorAlpha != 1 || def.Gatekeeper.MeritPriorBeta != 1 ||
		def.Gatekeeper.MeritLCBQuantile != 0.1 || def.Gatekeeper.MeritUnverifiedWeight != 0 {
		t.Fatalf("default posterior params = %+v", def.Gatekeeper)
	}
	for _, mode := range []string{"", config.MeritModeLegacy, config.MeritModePosteriorLCB, config.MeritModeThompson} {
		c := def
		c.Gatekeeper.MeritMode = mode
		if err := c.Validate(); err != nil {
			t.Errorf("mode %q rejected: %v", mode, err)
		}
	}
	for name, mutate := range map[string]func(*config.ExecutionConfig){
		"merit_mode":              func(c *config.ExecutionConfig) { c.Gatekeeper.MeritMode = "ucb" },
		"merit_prior_alpha":       func(c *config.ExecutionConfig) { c.Gatekeeper.MeritPriorAlpha = -1 },
		"merit_lcb_quantile":      func(c *config.ExecutionConfig) { c.Gatekeeper.MeritLCBQuantile = 1 },
		"merit_unverified_weight": func(c *config.ExecutionConfig) { c.Gatekeeper.MeritUnverifiedWeight = 2 },
	} {
		c := def
		mutate(&c)
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: err = %v, want a validation error naming it", name, err)
		}
	}
}
