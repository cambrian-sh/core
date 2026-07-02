package executer

import (
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// BenchmarkStepCache_KeyComputation measures SHA-256 key hashing cost.
func BenchmarkStepCache_KeyComputation(b *testing.B) {
	step := domain.Step{
		Query:     "summarise the Q3 revenue report and compare against Q2 targets",
		DependsOn: []int{0, 1, 2},
	}
	snapshot := map[string]string{
		"step_0_result": "revenue: $4.2M, up 12% QoQ",
		"step_1_result": "target was $4.0M for Q3",
		"step_2_result": "cost base: $2.1M, down 3% QoQ",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = stepCacheKey("finance analysis plan", "planid-abc123", step, snapshot)
	}
}
