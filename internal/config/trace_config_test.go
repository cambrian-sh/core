package config

import "testing"

// ADR-0145 D10: execution.trace defaults off, is reachable through the CAMBRIAN_*
// env names the use-case harness sets, and refuses "on" without a directory.
func TestTraceConfig(t *testing.T) {
	d := DefaultConfig().Execution
	if d.Trace.Enabled || d.Trace.ContentCap() != 20000 {
		t.Fatalf("defaults: %+v cap=%d", d.Trace, d.Trace.ContentCap())
	}
	dir := baseBundle(t)
	t.Setenv("CAMBRIAN_EXECUTION__TRACE__ENABLED", "true")
	t.Setenv("CAMBRIAN_EXECUTION__TRACE__DIR", "C:/tmp/traces")
	t.Setenv("CAMBRIAN_EXECUTION__TRACE__MAX_CONTENT_CHARS", "500")
	t.Setenv("CAMBRIAN_EXECUTION__PLAN__STEP_CACHE_DEFAULT_TTL_MINUTES", "5")
	cfg, _ := loadFrom(t, dir, nil)
	tr := cfg.Execution.Trace
	if !tr.Enabled || tr.Dir != "C:/tmp/traces" || tr.ContentCap() != 500 {
		t.Fatalf("env did not reach execution.trace: %+v", tr)
	}
	if cfg.Execution.Plan.StepCacheDefaultTTLMinutes != 5 {
		t.Fatalf("env did not reach step_cache_default_ttl_minutes: %d", cfg.Execution.Plan.StepCacheDefaultTTLMinutes)
	}
	if errs := (TraceConfig{Enabled: true}).validate(); len(errs) != 1 {
		t.Fatalf("enabled without dir must be refused: %v", errs)
	}
}
