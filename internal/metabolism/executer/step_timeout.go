package executer

import "time"

// stepTimeout computes the per-step execution deadline.
//
// Formula: (bidLatencyMs * multiplier) + baseBufferMs
// Both operands are sourced from config.ExecutionConfig so the formula is
// never hardcoded at the call site.
func stepTimeout(bidLatencyMs int, multiplier float64, baseBufferMs int) time.Duration {
	ms := float64(bidLatencyMs)*multiplier + float64(baseBufferMs)
	return time.Duration(ms) * time.Millisecond
}
