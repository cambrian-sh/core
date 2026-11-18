package config

import (
	"fmt"
	"math"
)

// ContentConfig is execution.content: every limit on how much CONTENT (a tool
// result, a re-opened reference, an upstream step's result) travels into a model's
// prompt, in one block, so the kernel and the agents it spawns size content by one
// policy (owner decision 2026-11-12,
// T-20261112-worker-resolve-cid-inline-has-no-size-cap-a and
// T-20261112-summarise-a-document-step-gets-only-the-first).
//
// Before this block the numbers were scattered and unrelated: a hardcoded 65536-byte
// inline threshold (app.go), ledger_get's 16000-byte page (execution.ledger.page_bytes),
// the SDK's own 24000-char tool-output cap and 600-char preview, and a fixed
// 24000-byte, head-only dependency budget (execution.plan.dependency_results_max_chars)
// that equalled the SDK cap only by coincidence of defaults.
//
// Every field is read: InlineResultBytes and PreviewChars by the ToolExecutor
// (app.go), PageBytes by ledger_get (kernel.NewLedgerStack) and the step-dependency
// outline, the dependency budget by the DAG executor (server.go), and
// ToolOutputMaxChars / PreviewChars / PageBytes / WorkerPromptBudgetTokens are served
// to every spawned agent as CAMBRIAN_CONTEXT_* (kernel.contentEnv), the variables the
// SDK's ContextConfig.from_env reads. Zero values take the documented defaults, so an
// existing tuning.json needs no new keys.
type ContentConfig struct {
	// InlineResultBytes: a tool result larger than this travels as its ledger
	// reference plus a summary (size, content type, shape, preview, how to read it)
	// instead of inline. 0 ⇒ 65536.
	InlineResultBytes int `json:"inline_result_bytes,omitempty"`
	// PreviewChars bounds the preview an offloaded result carries, and the SDK's
	// head preview of an over-long tool output (tool_output_preview_chars). A tool
	// result longer than this also carries its ledger reference when the ledger is
	// on, so a worker can answer with it (answer_ref) instead of re-typing it.
	// 0 ⇒ 600.
	PreviewChars int `json:"preview_chars,omitempty"`
	// PageBytes is one page of a long body: ledger_get's page, the window
	// resolve_cid returns, and the page numbers a shortened step dependency's outline
	// names. Replaces execution.ledger.page_bytes (migrated on load). 0 ⇒ 16000.
	PageBytes int `json:"page_bytes,omitempty"`
	// ToolOutputMaxChars: a single tool output longer than this is compacted in
	// the worker at once (head preview + reference). Must be >= PageBytes, or a
	// re-opened page would itself be compacted again (the p12 compaction/resolve
	// ping-pong). 0 ⇒ 24000.
	ToolOutputMaxChars int `json:"tool_output_max_chars,omitempty"`
	// WorkerPromptBudgetTokens is the worker's prompt budget: the SDK context
	// engine compacts above it (budget_tokens), and the step-dependency budget is a
	// fraction of it, so a dependent step's inputs are sized from what the step
	// that receives them can hold. 0 ⇒ 32000. execution.lane.context.budget_tokens,
	// when set, overrides it everywhere (ExecutionConfig.EffectiveContent).
	WorkerPromptBudgetTokens int `json:"worker_prompt_budget_tokens,omitempty"`
	// DependencyBudgetFraction is the share of WorkerPromptBudgetTokens a dependent
	// step's upstream results may take in total. 0 ⇒ 0.5 (16000 tokens by default,
	// so a 37k-character report arrives whole).
	DependencyBudgetFraction float64 `json:"dependency_budget_fraction,omitempty"`
	// DependencyMaxBytes is an optional hard ceiling, in bytes, on the same
	// budget. 0 ⇒ none (the token budget alone). An explicit
	// execution.plan.dependency_results_max_chars (the old fixed budget, also
	// bytes) is migrated here on load, so an operator's stated cap still holds.
	DependencyMaxBytes int `json:"dependency_max_bytes,omitempty"`
	// KeepRecentToolResults is the ADR-0150 D7 age trigger of the SDK context
	// engine: a tool result older than this many tool rounds becomes its
	// placeholder (naming its ledger id when it has one) even when the prompt is
	// under budget. 0 = off (only the budget trigger clears). Served as
	// CAMBRIAN_CONTEXT_KEEP_RECENT_TOOL_RESULTS. execution.lane.context.
	// keep_recent_tool_results, when set, overrides it (lane_context.go).
	KeepRecentToolResults int `json:"keep_recent_tool_results,omitempty"`
}

// CharsPerToken is the estimate the kernel and the SDK both use to turn a token
// budget into a size (llm.EstimateTokens, sdk context_engine.CHARS_PER_TOKEN) for
// ordinary text. High-entropy runs are counted at HighEntropyCharsPerToken.
const CharsPerToken = 4

// HighEntropyCharsPerToken and HighEntropyMinRun make the token estimate
// content-aware (T-20261112-pre-send-context-window-check-per-model-with): a
// maximal run of at least HighEntropyMinRun characters of the base64 / base64url /
// hex alphabet that mixes at least two of lower case, upper case and digits is
// counted at 1.3 characters per token instead of 4. len/4 under-counted base64
// 2-3x (run 20261112T132056Z p06: 100011 estimated, ~278483 counted by the
// provider, i.e. ~1.44 chars/token); 1.3 errs on the high side, which is the safe
// side for the gateway's pre-send context-window check. A provider count_tokens
// call was rejected: it is a network round-trip on every call's hot path and
// OpenAI-compatible endpoints have none. The SDK mirrors both constants
// (cambrian_sdk context_engine.HIGH_ENTROPY_*), with the same test vectors.
const (
	HighEntropyCharsPerToken = 1.3
	HighEntropyMinRun        = 64
)

// Inline is InlineResultBytes with its default applied.
func (c ContentConfig) Inline() int { return orDefault(c.InlineResultBytes, 65536) }

// Preview is PreviewChars with its default applied.
func (c ContentConfig) Preview() int { return orDefault(c.PreviewChars, 600) }

// Page is PageBytes with its default applied.
func (c ContentConfig) Page() int { return orDefault(c.PageBytes, 16000) }

// ToolOutputMax is ToolOutputMaxChars with its default applied.
func (c ContentConfig) ToolOutputMax() int { return orDefault(c.ToolOutputMaxChars, 24000) }

// WorkerPromptBudget is WorkerPromptBudgetTokens with its default applied.
func (c ContentConfig) WorkerPromptBudget() int { return orDefault(c.WorkerPromptBudgetTokens, 32000) }

// DependencyBudgetTokens is the token budget of a dependent step's upstream results.
func (c ContentConfig) DependencyBudgetTokens() int {
	f := c.DependencyBudgetFraction
	if f <= 0 {
		f = 0.5
	}
	return int(math.Floor(f * float64(c.WorkerPromptBudget())))
}

// DependencyBudgetBytes is DependencyBudgetTokens as bytes (CharsPerToken), capped
// by DependencyMaxBytes when set: what domain.FitStepDependencies is handed.
func (c ContentConfig) DependencyBudgetBytes() int {
	b := c.DependencyBudgetTokens() * CharsPerToken
	if c.DependencyMaxBytes > 0 && c.DependencyMaxBytes < b {
		return c.DependencyMaxBytes
	}
	return b
}

func orDefault(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// validate checks execution.content.
func (c ContentConfig) validate() []string {
	var errs []string
	for _, f := range []struct {
		name string
		v    int
	}{
		{"inline_result_bytes", c.InlineResultBytes},
		{"preview_chars", c.PreviewChars},
		{"page_bytes", c.PageBytes},
		{"tool_output_max_chars", c.ToolOutputMaxChars},
		{"worker_prompt_budget_tokens", c.WorkerPromptBudgetTokens},
		{"dependency_max_bytes", c.DependencyMaxBytes},
		{"keep_recent_tool_results", c.KeepRecentToolResults},
	} {
		if f.v < 0 {
			errs = append(errs, fmt.Sprintf("execution.content.%s must be >= 0 (got %d)", f.name, f.v))
		}
	}
	if c.DependencyBudgetFraction < 0 || c.DependencyBudgetFraction > 1 {
		errs = append(errs, fmt.Sprintf("execution.content.dependency_budget_fraction must be in [0, 1] (got %g)", c.DependencyBudgetFraction))
	}
	if len(errs) > 0 {
		return errs
	}
	// The effective values must nest: a page that is itself over the worker's
	// tool-output cap is compacted the moment it is re-opened, and a preview as long
	// as a page is no preview.
	if c.Page() > c.ToolOutputMax() {
		errs = append(errs, fmt.Sprintf("execution.content.page_bytes (%d) must not exceed tool_output_max_chars (%d): a re-opened page would be compacted again", c.Page(), c.ToolOutputMax()))
	}
	if c.Preview() >= c.Page() {
		errs = append(errs, fmt.Sprintf("execution.content.preview_chars (%d) must be smaller than page_bytes (%d)", c.Preview(), c.Page()))
	}
	return errs
}
