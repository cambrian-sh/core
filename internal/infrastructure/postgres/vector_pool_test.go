package postgres

import (
	"testing"

	"github.com/cambrian-sh/core/internal/config"
)

// ENG-210: the dense candidate pool is top_k × retrieval.vector_pool_multiplier,
// at least 10, at most retrieval.vector_pool_max — and with the defaults it is
// exactly the pool the adapter hardcoded before (20x, min 10, cap 3000).
func TestCandidatePool_DefaultIsTheOldSizing(t *testing.T) {
	old := func(topK int) int { // the pre-ENG-210 inline code
		n := topK * 20
		if n < 10 {
			n = 10
		}
		if n > 3000 {
			n = 3000
		}
		if topK <= 0 {
			n = 10
		}
		return n
	}
	m, mx := config.DefaultConfig().Execution.Retrieval.EffectiveVectorPool()
	configured := &PgVectorAdapter{poolMultiplier: m, poolMax: mx}
	literal := &PgVectorAdapter{} // zero values ⇒ defaults
	for _, k := range []int{-1, 0, 1, 5, 10, 100, 149, 150, 151, 1000} {
		if got := configured.candidatePool(k); got != old(k) {
			t.Errorf("default config, top_k %d: pool %d, want %d", k, got, old(k))
		}
		if got := literal.candidatePool(k); got != old(k) {
			t.Errorf("zero-value adapter, top_k %d: pool %d, want %d", k, got, old(k))
		}
	}
	p := &PgVectorAdapter{poolMultiplier: 3, poolMax: 50}
	for k, want := range map[int]int{1: 10, 5: 15, 100: 50, 0: 10} {
		if got := p.candidatePool(k); got != want {
			t.Errorf("3x cap 50, top_k %d: pool %d, want %d", k, got, want)
		}
	}
}
