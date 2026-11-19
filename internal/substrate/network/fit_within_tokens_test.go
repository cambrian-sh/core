package network

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cambrian-sh/core/internal/infrastructure/llm"
)

// fitWithinTokens bounds the chunk that crosses the Pass-1 cut-off (ENG-2234).
func TestFitWithinTokens(t *testing.T) {
	hex := strings.Repeat("0123456789abcdef", 8)
	cases := []struct {
		name, prev, text string
		limit            int
	}{
		{"ordinary text", strings.Repeat("word ", 30), "more words here and there", 40},
		{"dense run crossing the min-run jump", hex[:48], hex[48:], 40},
		{"multibyte runes", strings.Repeat("ab ", 50), strings.Repeat("çğış", 20), 45},
		{"prefix already at the limit", strings.Repeat("abcd", 40), "efgh", 40},
		{"empty text", "abc", "", 10},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := fitWithinTokens(c.prev, c.text, c.limit)
			if !strings.HasPrefix(c.text, got) {
				t.Fatalf("result %q is not a prefix of %q", got, c.text)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("result cut a rune: %q", got)
			}
			if n := llm.EstimateTokens(c.prev + got); n > c.limit && got != "" {
				t.Fatalf("prev+result estimates %d tokens, limit %d", n, c.limit)
			}
		})
	}
	// When the whole text fits it is returned unchanged.
	if got := fitWithinTokens("", "short", 100); got != "short" {
		t.Fatalf("fitting text altered: %q", got)
	}
}
