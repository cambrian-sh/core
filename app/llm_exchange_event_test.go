package app

import (
	"strings"
	"testing"
)

// REGRESSION (ENG-1317): PromptChars/ResponseChars were byte counts while the text was
// truncated in runes, so non-ASCII text below the cap read as truncated. Counts and
// truncation now share one unit (runes).
func TestLLMExchangeEvent_CountsRunesLikeTheTruncation(t *testing.T) {
	t.Parallel()
	prompt := strings.Repeat("ş", 10)     // 10 runes, 20 bytes
	completion := strings.Repeat("ğ", 30) // 30 runes, 60 bytes
	e := llmExchangeEvent("s", "a", "m", 1, prompt, completion, 20)
	if e.PromptChars != 10 || e.ResponseChars != 30 {
		t.Fatalf("counts must be runes: prompt=%d response=%d", e.PromptChars, e.ResponseChars)
	}
	if e.Prompt != prompt {
		t.Fatalf("a 10-rune prompt under a 20-rune cap must not be truncated: %q", e.Prompt)
	}
	if !strings.HasPrefix(e.Completion, strings.Repeat("ğ", 20)) || !strings.HasSuffix(e.Completion, "[truncated]") {
		t.Fatalf("completion must be cut at 20 runes: %q", e.Completion)
	}
}
