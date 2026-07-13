package config_test

import (
	"testing"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/domain"
)

// Cycle 1 — GetPolicy returns (policy, true) for a known name.
func TestStaticPolicyProvider_GetPolicy_KnownName(t *testing.T) {
	p := config.NewStaticPolicyProvider(
		map[string]domain.HippocampusPolicy{
			"codegen": {SimilarityThreshold: 0.92, ConfidenceFloor: 0.85, MaxAgeHours: 24},
		},
		"codegen",
	)
	got, ok := p.GetPolicy("codegen")
	if !ok {
		t.Fatal("expected ok=true for known policy name")
	}
	if got.SimilarityThreshold != 0.92 {
		t.Errorf("SimilarityThreshold: want 0.92 got %v", got.SimilarityThreshold)
	}
	if got.ConfidenceFloor != 0.85 {
		t.Errorf("ConfidenceFloor: want 0.85 got %v", got.ConfidenceFloor)
	}
	if got.MaxAgeHours != 24 {
		t.Errorf("MaxAgeHours: want 24 got %v", got.MaxAgeHours)
	}
}

// Cycle 2 — GetPolicy returns (zero, false) for an unknown name.
func TestStaticPolicyProvider_GetPolicy_UnknownName(t *testing.T) {
	p := config.NewStaticPolicyProvider(
		map[string]domain.HippocampusPolicy{
			"default": {SimilarityThreshold: 0.85},
		},
		"default",
	)
	got, ok := p.GetPolicy("nonexistent")
	if ok {
		t.Fatalf("expected ok=false for unknown policy, got policy=%+v", got)
	}
	if got != (domain.HippocampusPolicy{}) {
		t.Errorf("expected zero HippocampusPolicy on miss, got %+v", got)
	}
}

// Cycle 3 — DefaultPolicy returns the policy named by HippocampusDefaultPolicy.
func TestStaticPolicyProvider_DefaultPolicy(t *testing.T) {
	policies := map[string]domain.HippocampusPolicy{
		"cognitive": {SimilarityThreshold: 0.85, ConfidenceFloor: 0.70, MaxAgeHours: 168},
		"default":   {SimilarityThreshold: 0.85, ConfidenceFloor: 0.70, MaxAgeHours: 168},
	}
	p := config.NewStaticPolicyProvider(policies, "cognitive")
	got := p.DefaultPolicy()
	if got.SimilarityThreshold != 0.85 {
		t.Errorf("DefaultPolicy SimilarityThreshold: want 0.85 got %v", got.SimilarityThreshold)
	}
}

// Cycle 4 — DefaultConfig populates all 5 expected policies with correct key values.
func TestDefaultConfig_HippocampusPolicies_AllPresent(t *testing.T) {
	cfg := config.DefaultConfig()
	policies := cfg.Execution.HippocampusPolicies

	cases := []struct {
		name      string
		wantSim   float64
		wantConf  float64
		wantAge   int
	}{
		{"codegen", 0.92, 0.85, 24},
		{"cognitive", 0.85, 0.70, 168},
		{"tool", 0.80, 0.60, 720},
		{"research", 0.88, 0.75, 72},
		{"default", 0.85, 0.70, 168},
	}
	for _, tc := range cases {
		pol, ok := policies[tc.name]
		if !ok {
			t.Errorf("DefaultConfig missing policy %q", tc.name)
			continue
		}
		if pol.SimilarityThreshold != tc.wantSim {
			t.Errorf("policy %q SimilarityThreshold: want %v got %v", tc.name, tc.wantSim, pol.SimilarityThreshold)
		}
		if pol.ConfidenceFloor != tc.wantConf {
			t.Errorf("policy %q ConfidenceFloor: want %v got %v", tc.name, tc.wantConf, pol.ConfidenceFloor)
		}
		if pol.MaxAgeHours != tc.wantAge {
			t.Errorf("policy %q MaxAgeHours: want %v got %v", tc.name, tc.wantAge, pol.MaxAgeHours)
		}
	}
	if cfg.Execution.HippocampusDefaultPolicy != "default" {
		t.Errorf("HippocampusDefaultPolicy: want %q got %q", "default", cfg.Execution.HippocampusDefaultPolicy)
	}
}

// Cycle 4b — DefaultConfig contains "episodic" policy with ADR-0029 values.
func TestDefaultConfig_EpisodicPolicy_Present(t *testing.T) {
	cfg := config.DefaultConfig()
	pol, ok := cfg.Execution.HippocampusPolicies["episodic"]
	if !ok {
		t.Fatal("DefaultConfig missing 'episodic' HippocampusPolicy (ADR-0029)")
	}
	if pol.SimilarityThreshold != 0.65 {
		t.Errorf("episodic SimilarityThreshold: want 0.65 got %v", pol.SimilarityThreshold)
	}
	if pol.MaxAgeHours != 8760 {
		t.Errorf("episodic MaxAgeHours: want 8760 (1 year) got %v", pol.MaxAgeHours)
	}
}

// Cycle 4c — DefaultConfig.EpisodicConsolidationDelayMs defaults to 300_000 (5 min).
func TestDefaultConfig_EpisodicConsolidationDelayMs_Default(t *testing.T) {
	cfg := config.DefaultConfig()
	if cfg.Execution.EpisodicConsolidationDelayMs != 300_000 {
		t.Errorf("EpisodicConsolidationDelayMs: want 300000 got %d", cfg.Execution.EpisodicConsolidationDelayMs)
	}
}

// Cycle 5 — Validate rejects a config where HippocampusDefaultPolicy is not in the map.
func TestExecutionConfig_Validate_RejectsMissingDefaultPolicy(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Execution.HippocampusDefaultPolicy = "nonexistent"

	err := cfg.Execution.Validate()
	if err == nil {
		t.Fatal("expected Validate to return error when default policy key is absent from map")
	}
}

// Cycle 6 — Validate accepts a config where HippocampusDefaultPolicy IS in the map.
func TestExecutionConfig_Validate_AcceptsValidDefaultPolicy(t *testing.T) {
	cfg := config.DefaultConfig()
	// DefaultConfig sets "default" which exists in HippocampusPolicies.
	if err := cfg.Execution.Validate(); err != nil {
		t.Fatalf("DefaultConfig should pass Validate: %v", err)
	}
}
