package config

import (
	"strings"
	"testing"
)

// ADR-0136 A3.1: the descriptive source-metadata caps live in the Ingestion block,
// default 64/256/128/64, reach the fields from a file and from CAMBRIAN_* env vars, and
// a negative value is a validation error (0 = the default).
func TestSourceMetaCapsKeys(t *testing.T) {
	ing := DefaultConfig().Execution.Ingestion
	if c := ing.SourceMetaCaps(); c != (SourceMetaCaps{Kind: 64, Title: 256, Author: 128, Speaker: 64}) {
		t.Fatalf("default caps = %+v", c)
	}
	dir := baseBundle(t)
	writeCfg(t, dir, "tuning.json", `{"schema_version":2,"execution":{"ingestion":{"source_kind_max_chars":10,"source_title_max_chars":20,"source_author_max_chars":30,"part_speaker_max_chars":40}}}`)
	cfg, _ := loadFrom(t, dir, nil)
	if c := cfg.Execution.Ingestion.SourceMetaCaps(); c != (SourceMetaCaps{Kind: 10, Title: 20, Author: 30, Speaker: 40}) {
		t.Fatalf("file caps = %+v", c)
	}
	dir = baseBundle(t)
	t.Setenv("CAMBRIAN_EXECUTION__INGESTION__PART_SPEAKER_MAX_CHARS", "7")
	cfg, _ = loadFrom(t, dir, nil)
	if cfg.Execution.Ingestion.PartSpeakerMaxChars != 7 {
		t.Fatalf("env cap = %d, want 7", cfg.Execution.Ingestion.PartSpeakerMaxChars)
	}
	bad := DefaultConfig().Execution
	bad.Ingestion.SourceTitleMaxChars = -1
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "source_title_max_chars") {
		t.Fatalf("negative cap: err = %v", err)
	}
}
