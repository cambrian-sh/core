package llm

import "sync"

var (
	mu             sync.Mutex
	totalCalls     int64
	budgetOverruns int64
)

// RecordTokenCall records one GenerateViaModelStream completion for
// TOKENIZER_INACCURACY telemetry. budgetOverrun=true means the reconciled
// ActualTokensUsed exceeded TokenLimit.
func RecordTokenCall(budgetOverrun bool) {
	mu.Lock()
	defer mu.Unlock()
	totalCalls++
	if budgetOverrun {
		budgetOverruns++
	}
}

// CheckTokenizerInaccuracy returns the current budget overrun rate
// (BudgetOverrun events / total calls) for TOKENIZER_INACCURACY telemetry.
// If totalCalls==0, returns 0.
func CheckTokenizerInaccuracy() float64 {
	mu.Lock()
	defer mu.Unlock()
	if totalCalls == 0 {
		return 0
	}
	return float64(budgetOverruns) / float64(totalCalls)
}

// ResetTokenizerInaccuracyCounter zeroes all counters. Test-only; not for production use.
func ResetTokenizerInaccuracyCounter() {
	mu.Lock()
	defer mu.Unlock()
	totalCalls = 0
	budgetOverruns = 0
}
