package config

import "testing"

func TestSkillsConfig_DefaultsAndValidate(t *testing.T) {
	d := DefaultConfig().Execution.Skills
	if !d.ProposalsEnabled || d.Curator.Enabled || d.Curator.StaleAfterDays != 30 || d.Curator.ArchiveAfterDays != 90 {
		t.Fatalf("defaults = %+v (want proposals on, curator off, 30/90 days)", d)
	}
	if errs := d.validate(); len(errs) != 0 {
		t.Fatalf("defaults invalid: %v", errs)
	}
	bad := d
	bad.Curator.Enabled = true
	bad.Curator.ArchiveAfterDays = 10
	bad.Curator.Sources = []string{"agent-proposed", "nonsense"}
	if errs := bad.validate(); len(errs) != 2 {
		t.Fatalf("want archive<=stale and unknown-source errors, got %v", errs)
	}
}

// The block is read from a file and from CAMBRIAN_* env vars (nested keys).
func TestLoadConfig_SkillsKeys(t *testing.T) {
	dir := baseBundle(t)
	writeCfg(t, dir, "tuning.json", `{"schema_version":2,"execution":{"skills":{"max_pending_per_agent":5,"curator":{"pinned":["a"]}}}}`)
	cfg, _ := loadFrom(t, dir, nil)
	if cfg.Execution.Skills.MaxPendingPerAgent != 5 || len(cfg.Execution.Skills.Curator.Pinned) != 1 || !cfg.Execution.Skills.ProposalsEnabled {
		t.Fatalf("file keys did not reach the fields: %+v", cfg.Execution.Skills)
	}
	dir = baseBundle(t)
	t.Setenv("CAMBRIAN_EXECUTION__SKILLS__CURATOR__ENABLED", "true")
	t.Setenv("CAMBRIAN_EXECUTION__SKILLS__CURATOR__STALE_AFTER_DAYS", "7")
	cfg, _ = loadFrom(t, dir, nil)
	if !cfg.Execution.Skills.Curator.Enabled || cfg.Execution.Skills.Curator.StaleAfterDays != 7 {
		t.Fatalf("env keys did not reach the fields: %+v", cfg.Execution.Skills.Curator)
	}
}
