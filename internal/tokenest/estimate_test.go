package tokenest

import (
	"strings"
	"testing"
)

// The rule itself (the llm package keeps its case table through
// llm.EstimateTokens, which delegates here): ordinary text at 4 chars/token, a
// high-entropy run at 1.3, never 0 for non-empty text.
func TestEstimate(t *testing.T) {
	cases := []struct {
		name string
		text string
		want int
	}{
		{"empty", "", 0},
		{"short", "hi", 1},
		{"prose", strings.Repeat("word ", 80), 100},
		{"base64 run", strings.Repeat("a1B2", 64), 197}, // ceil(256/1.3)
		{"run too short", strings.Repeat("a1B2", 15), 15},
	}
	for _, c := range cases {
		if got := Estimate(c.text); got != c.want {
			t.Errorf("%s: Estimate = %d, want %d", c.name, got, c.want)
		}
	}
}
