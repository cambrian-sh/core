package kernel

import (
	"slices"
	"testing"

	"github.com/cambrian-sh/core/internal/config"
)

// execution.content reaches every spawned agent as the CAMBRIAN_CONTEXT_* variables
// the SDK's ContextConfig.from_env reads, defaults included.
func TestContentEnv_ServesTheSDKLimits(t *testing.T) {
	got := contentEnv(config.ExecutionConfig{})
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
	got = contentEnv(config.ExecutionConfig{Content: config.ContentConfig{PageBytes: 8000, WorkerPromptBudgetTokens: 64000, KeepRecentToolResults: 6}})
	if !slices.Contains(got, "CAMBRIAN_CONTEXT_KEEP_RECENT_TOOL_RESULTS=6") {
		t.Errorf("keep_recent_tool_results not served: %v", got)
	}
	if !slices.Contains(got, "CAMBRIAN_CONTEXT_RESOLVE_PAGE_CHARS=8000") || !slices.Contains(got, "CAMBRIAN_CONTEXT_BUDGET_TOKENS=64000") {
		t.Errorf("configured values not served: %v", got)
	}
}

// ENG-595 (owner decision 2026-11-14): execution.lane.context wins over
// execution.content in the env the kernel serves, so the env channel and the
// handoff stamp agree; an unset lane key leaves execution.content's value.
func TestContentEnv_LaneContextWins(t *testing.T) {
	e := config.ExecutionConfig{
		Content: config.ContentConfig{WorkerPromptBudgetTokens: 64000, KeepRecentToolResults: 6},
		Lane:    config.LaneConfig{Context: config.LaneContextConfig{BudgetTokens: 48000, ProtectTail: 5, ToolOutputCompactChars: 900}},
	}
	got := contentEnv(e)
	for _, want := range []string{
		"CAMBRIAN_CONTEXT_BUDGET_TOKENS=48000",           // lane wins
		"CAMBRIAN_CONTEXT_KEEP_RECENT_TOOL_RESULTS=6",    // lane unset: content stands
		"CAMBRIAN_CONTEXT_PROTECT_TAIL=5",                // lane-only key served
		"CAMBRIAN_CONTEXT_TOOL_OUTPUT_COMPACT_CHARS=900", // lane-only key served
	} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	if slices.Contains(got, "CAMBRIAN_CONTEXT_BUDGET_TOKENS=64000") {
		t.Errorf("execution.content budget served although the lane block sets it: %v", got)
	}
	// The handoff carries only the lane's set keys, with the same values.
	lim := e.AgentContext()
	if lim.BudgetTokens != 48000 || lim.KeepRecentToolResults != 0 || lim.ProtectTail != 5 || lim.ToolOutputCompactChars != 900 {
		t.Errorf("AgentContext = %+v", lim)
	}
	if b := e.EffectiveContent().WorkerPromptBudget(); b != 48000 {
		t.Errorf("dependency budget sized from %d, want the lane's 48000", b)
	}
}
