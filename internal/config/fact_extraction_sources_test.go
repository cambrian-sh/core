package config

import (
	"strings"
	"testing"
)

// ENG-494: execution.memory.fact_extraction_sources names writer kinds or one
// connector; anything else fails boot rather than silently extracting nothing.
func TestFactExtractionSources_Validated(t *testing.T) {
	c := DefaultConfig().Execution
	if len(c.Memory.FactExtractionSources) != 0 {
		t.Fatalf("default must be empty (extract every ingest, the pre-guard behaviour): %v", c.Memory.FactExtractionSources)
	}
	c.Memory.FactExtractionSources = []string{"agent", "user", "source", "source:telegram"}
	if err := c.Validate(); err != nil && strings.Contains(err.Error(), "fact_extraction_sources") {
		t.Fatalf("valid entries refused: %v", err)
	}
	for _, bad := range []string{"", "telegram", "source:", "source:a:b", "Agent"} {
		c.Memory.FactExtractionSources = []string{bad}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "fact_extraction_sources") {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
}
