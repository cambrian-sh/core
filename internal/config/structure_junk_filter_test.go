package config

import (
	"slices"
	"strings"
	"testing"
)

// T-20261102-ingestion-structure-junk-filter-silently-drops: the default drops
// junk only from Docling output, and an unknown backend name is rejected rather
// than silently matching nothing.
func TestStructureJunkFilterBackends_DefaultAndValidation(t *testing.T) {
	cfg := DefaultConfig()
	if got := cfg.Execution.Ingestion.StructureJunkFilterBackends; !slices.Equal(got, []string{"docling"}) {
		t.Fatalf("default = %v, want [docling]", got)
	}
	if err := cfg.Execution.Validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
	cfg.Execution.Ingestion.StructureJunkFilterBackends = []string{"docling", "pdf"}
	err := cfg.Execution.Validate()
	if err == nil || !strings.Contains(err.Error(), "structure_junk_filter_backends") {
		t.Fatalf("unknown backend not rejected: %v", err)
	}
	cfg.Execution.Ingestion.StructureJunkFilterBackends = nil
	if err := cfg.Execution.Validate(); err != nil {
		t.Fatalf("empty list (filter off) must be valid: %v", err)
	}
}
