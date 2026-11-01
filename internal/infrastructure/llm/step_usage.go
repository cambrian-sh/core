package llm

import (
	"strconv"

	"github.com/cambrian-sh/core/domain"
)

// Step usage on the handoff (ADR-0131 D2;
// T-20261101-step-taskevents-still-bill-len-4-although-the).
//
// The step function owns the step's BudgetLease and learns its reconciled
// provider usage from the gateway's Complete; the DAGExecutor writes the step's
// TaskEvent. The usage crosses from one to the other on the step response's
// kernel-internal context keys (domain.HandoffKeyStep*), the same channel
// BudgetOverrun already rides. These two helpers are the only writer and the
// only reader of those keys.

var stepUsageKeys = [...]string{
	domain.HandoffKeyStepPromptTokens,
	domain.HandoffKeyStepCompletionTokens,
	domain.HandoffKeyStepCacheReadTokens,
	domain.HandoffKeyStepUsageEstimated,
}

// StampStepUsage writes the step lease's usage onto a step response context.
// It first deletes any value already there — the keys are kernel-authoritative
// and an agent echoing its input context must not be able to set them — and
// stamps nothing for a zero usage (the step made no LLM call through the
// gateway), so the executor falls back to its flagged estimate.
func StampStepUsage(ctx map[string]string, u TokenUsage) {
	if ctx == nil {
		return
	}
	ClearStepUsage(ctx)
	if u.IsZero() {
		return
	}
	ctx[domain.HandoffKeyStepPromptTokens] = strconv.Itoa(u.PromptTokens)
	ctx[domain.HandoffKeyStepCompletionTokens] = strconv.Itoa(u.CompletionTokens)
	ctx[domain.HandoffKeyStepCacheReadTokens] = strconv.Itoa(u.CacheReadTokens)
	if u.Estimated {
		ctx[domain.HandoffKeyStepUsageEstimated] = "true"
	}
}

// ClearStepUsage deletes the step-usage keys from ctx.
func ClearStepUsage(ctx map[string]string) {
	for _, k := range stepUsageKeys {
		delete(ctx, k)
	}
}

// StepUsageFrom reads the usage StampStepUsage wrote. ok is false when no
// usage was stamped (or it does not parse), in which case the caller uses its
// own flagged estimate.
func StepUsageFrom(ctx map[string]string) (u TokenUsage, ok bool) {
	p, perr := strconv.Atoi(ctx[domain.HandoffKeyStepPromptTokens])
	c, cerr := strconv.Atoi(ctx[domain.HandoffKeyStepCompletionTokens])
	if perr != nil || cerr != nil || p < 0 || c < 0 || p+c == 0 {
		return TokenUsage{}, false
	}
	cr, _ := strconv.Atoi(ctx[domain.HandoffKeyStepCacheReadTokens]) // absent = 0
	return TokenUsage{
		PromptTokens:     p,
		CompletionTokens: c,
		TotalTokens:      p + c,
		CacheReadTokens:  cr,
		Estimated:        ctx[domain.HandoffKeyStepUsageEstimated] == "true",
	}, true
}
