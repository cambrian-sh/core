package config

import (
	"strings"
	"testing"
)

// ENG-117: execution.tools.output_scan is on by default (hit at dangerous,
// flag) and its enumerations are validated.
func TestToolOutputScan_DefaultsAndValidate(t *testing.T) {
	d := DefaultConfig().Execution.Tools.OutputScan
	if !d.Enabled || d.HitAtOrDefault() != "dangerous" || d.OnHitOrDefault() != "flag" {
		t.Fatalf("defaults: %+v", d)
	}
	if errs := d.validate(); len(errs) != 0 {
		t.Fatalf("default invalid: %v", errs)
	}
	for _, bad := range []ToolOutputScanConfig{
		{Enabled: true, HitAt: "clean"},
		{Enabled: true, OnHit: "approve"},
		{Enabled: true, ExtraRules: []ScanRuleConfig{{ID: "X", Severity: "danger"}}},
		{Enabled: true, ExtraRules: []ScanRuleConfig{{ID: "X", Pattern: "x", Severity: "high"}}},
	} {
		if len(bad.validate()) == 0 {
			t.Errorf("accepted %+v", bad)
		}
	}
	cfg := DefaultConfig()
	cfg.Execution.Tools.OutputScan.OnHit = "approve"
	if err := cfg.Execution.Validate(); err == nil || !strings.Contains(err.Error(), "output_scan.on_hit") {
		t.Fatalf("ExecutionConfig.Validate does not check output_scan: %v", err)
	}
}
