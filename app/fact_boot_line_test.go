package app

import (
	"strings"
	"testing"
)

// ENG-490: the fact-layer boot line carries the extractor version and the A2 budgets,
// appended after the prefix the benchmark specs already anchor on.
func TestFactLayerBootLine_AppendsVersionAndBudgetsAfterTheAnchoredPrefix(t *testing.T) {
	t.Parallel()
	on := factLayerBootLine(factLayerBoot{
		Extract: true, Supersession: true, Assessments: true, Store: "substrate",
		ExtractorVersion: "fact-extract-v2/llm:haiku-local", MaxCallsPerDocument: 8, MaxFactsPerSource: 40, MaxPromptChars: 12000,
	})
	prefix := "fact layer: extract_enabled=true lane_enabled=false supersession_enabled=true assessments_enabled=true store=substrate"
	if !strings.HasPrefix(on, prefix) {
		t.Fatalf("recorded expect_log prefix no longer matches:\n%s", on)
	}
	for _, want := range []string{
		"extractor_version=fact-extract-v2/llm:haiku-local", "max_calls_per_document=8",
		"max_facts_per_source=40", "max_prompt_chars=12000",
	} {
		if !strings.Contains(on, want) {
			t.Errorf("boot line lacks %q:\n%s", want, on)
		}
	}
	off := factLayerBootLine(factLayerBoot{Store: "none"})
	if !strings.Contains(off, " extractor_version=none ") {
		t.Fatalf("extraction off must say extractor_version=none:\n%s", off)
	}
}
