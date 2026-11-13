package kernel

import (
	"slices"
	"testing"

	"github.com/cambrian-sh/core/internal/config"
)

// execution.content reaches every spawned agent as the CAMBRIAN_CONTEXT_* variables
// the SDK's ContextConfig.from_env reads, defaults included.
func TestContentEnv_ServesTheSDKLimits(t *testing.T) {
	got := contentEnv(config.ContentConfig{})
	for _, want := range []string{
		"CAMBRIAN_CONTEXT_TOOL_OUTPUT_MAX_CHARS=24000",
		"CAMBRIAN_CONTEXT_TOOL_OUTPUT_PREVIEW_CHARS=600",
		"CAMBRIAN_CONTEXT_RESOLVE_PAGE_CHARS=16000",
		"CAMBRIAN_CONTEXT_BUDGET_TOKENS=32000",
		"CAMBRIAN_CONTEXT_KEEP_RECENT_TOOL_RESULTS=0",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	got = contentEnv(config.ContentConfig{PageBytes: 8000, WorkerPromptBudgetTokens: 64000, KeepRecentToolResults: 6})
	if !slices.Contains(got, "CAMBRIAN_CONTEXT_KEEP_RECENT_TOOL_RESULTS=6") {
		t.Errorf("keep_recent_tool_results not served: %v", got)
	}
	if !slices.Contains(got, "CAMBRIAN_CONTEXT_RESOLVE_PAGE_CHARS=8000") || !slices.Contains(got, "CAMBRIAN_CONTEXT_BUDGET_TOKENS=64000") {
		t.Errorf("configured values not served: %v", got)
	}
}
