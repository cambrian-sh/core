package mockgen_test

import (
	"testing"
	"time"

	"github.com/cambrian-sh/core/internal/testing/mockgen"
)

func TestGenerateTelemetryCorpus_Baseline(t *testing.T) {
	events := mockgen.GenerateTelemetryCorpus(100, mockgen.Baseline, nil, 42)
	if len(events) != 100 {
		t.Fatalf("expected 100 events, got %d", len(events))
	}
	for _, e := range events {
		if e.TaskID == "" {
			t.Error("TaskID is empty")
		}
		if e.TotalTokens != e.PromptTokens+e.CompletionTokens {
			t.Errorf("TotalTokens %d != PromptTokens %d + CompletionTokens %d", e.TotalTokens, e.PromptTokens, e.CompletionTokens)
		}
	}
}

func TestGenerateTelemetryCorpus_BudgetCrisis_DerivedBudgetOverrun(t *testing.T) {
	events := mockgen.GenerateTelemetryCorpus(1000, mockgen.BudgetCrisis, nil, 42)
	var overrunCount int
	for _, e := range events {
		if e.BudgetOverrun {
			overrunCount++
			if e.CompletionTokens <= 300 {
				t.Errorf("BudgetOverrun=true but CompletionTokens=%d <= 300 (impossible)", e.CompletionTokens)
			}
		}
	}
	// BudgetCrisis has TokenLimitPerStep=300, CompletionTokensMean=400 → most should overrun
	rate := float64(overrunCount) / 1000
	if rate < 0.85 || rate > 0.99 {
		t.Errorf("BudgetOverrun rate = %.2f, want ~0.95 (limit=300, completion mean=400, stddev=60)", rate)
	}
}

func TestGenerateTelemetryCorpus_FallbackRate(t *testing.T) {
	events := mockgen.GenerateTelemetryCorpus(1000, mockgen.ColdStart, nil, 42)
	var fallbackCount int
	for _, e := range events {
		if e.FallbackModelUsed {
			fallbackCount++
		}
	}
	rate := float64(fallbackCount) / 1000
	if rate < 0.05 || rate > 0.25 {
		t.Errorf("FallbackModelUsed rate = %.2f, want ~0.15 (ColdStart)", rate)
	}
}

func TestGenerateTelemetryCorpus_Speed(t *testing.T) {
	n := 1000
	start := time.Now()
	events := mockgen.GenerateTelemetryCorpus(n, mockgen.Baseline, nil, 42)
	elapsed := time.Since(start)

	if len(events) != n {
		t.Fatalf("expected %d events, got %d", n, len(events))
	}
	if elapsed > 1*time.Second {
		t.Errorf("generating %d events took %v, want <1s", n, elapsed)
	}
}

func TestGenerateTelemetryCorpus_SeedDeterministic(t *testing.T) {
	a := mockgen.GenerateTelemetryCorpus(10, mockgen.Baseline, nil, 42)
	b := mockgen.GenerateTelemetryCorpus(10, mockgen.Baseline, nil, 42)
	for i := range a {
		if a[i].TotalTokens != b[i].TotalTokens {
			t.Fatalf("TotalTokens differ at index %d: %d vs %d (non-deterministic)", i, a[i].TotalTokens, b[i].TotalTokens)
		}
	}
}
