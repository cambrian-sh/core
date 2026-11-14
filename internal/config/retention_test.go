package config

import (
	"strings"
	"testing"
)

// ENG-128 (owner decision 2026-11-15): operator audit is kept 365 days by
// default; derived/cache TTLs are off by default.
func TestRetentionDefaults(t *testing.T) {
	rc := DefaultConfig().Execution.Retention
	if rc.OperatorAuditDays != 365 {
		t.Errorf("operator_audit_days default = %d, want 365", rc.OperatorAuditDays)
	}
	if rc.ChunkTripletsDays != 0 || rc.ExperiencesDays != 0 {
		t.Errorf("derived/cache TTLs must default off, got %+v", rc)
	}
	if rc.SweepIntervalSeconds <= 0 || rc.BatchSize <= 0 {
		t.Errorf("worker must run by default, got %+v", rc)
	}
}

func TestRetentionValidateRejectsNegatives(t *testing.T) {
	rc := DefaultRetentionConfig()
	rc.OperatorAuditDays = -1
	rc.BatchSize = -5
	errs := rc.validate()
	if len(errs) != 2 || !strings.Contains(strings.Join(errs, ";"), "execution.retention.operator_audit_days") {
		t.Fatalf("errs = %v", errs)
	}
}
