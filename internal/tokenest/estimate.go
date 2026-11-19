// Package tokenest is the kernel's one token estimate without a tokenizer
// (T-20261112-pre-send-context-window-check-per-model-with). It is a leaf — it
// imports only the config constants — so the LLM layer (llm.EstimateTokens, the
// spend meter, the context guard) and the chat transcript budget (internal/chat)
// count with the same rule instead of keeping copies (ENG-2127).
package tokenest

import (
	"math"
	"math/bits"

	"github.com/cambrian-sh/core/internal/config"
)

// Estimate returns a token count for text without a tokenizer.
//
// It is content-aware (T-20261112-pre-send-context-window-check-per-model-with):
// ordinary text counts at config.CharsPerToken (4) characters per token, while a
// high-entropy run — a maximal run of at least config.HighEntropyMinRun base64 /
// base64url / hex characters mixing at least two of lower case, upper case and
// digits — counts at config.HighEntropyCharsPerToken (1.3). Plain len/4
// under-counted base64 2-3x, so a prompt full of encoded payloads estimated well
// under a context window it overflowed. One pass over the bytes; no allocation.
// The SDK's context_engine.estimate_tokens implements the same rule.
func Estimate(text string) int {
	if len(text) == 0 {
		return 0
	}
	normal, entropy := splitEntropy(text)
	tokens := normal / config.CharsPerToken
	if entropy > 0 {
		tokens += int(math.Ceil(float64(entropy) / config.HighEntropyCharsPerToken))
	}
	if tokens == 0 {
		tokens = 1
	}
	return tokens
}

// splitEntropy returns how many bytes of text are ordinary and how many lie in
// high-entropy runs (see Estimate).
func splitEntropy(text string) (normal, entropy int) {
	start := -1
	var classes uint8
	flush := func(end int) {
		if start < 0 {
			return
		}
		if n := end - start; n >= config.HighEntropyMinRun && bits.OnesCount8(classes) >= 2 {
			entropy += n
		}
		start, classes = -1, 0
	}
	for i := 0; i < len(text); i++ {
		c := text[i]
		var class uint8
		switch {
		case c >= 'a' && c <= 'z':
			class = 1
		case c >= 'A' && c <= 'Z':
			class = 2
		case c >= '0' && c <= '9':
			class = 4
		case c == '+' || c == '/' || c == '=' || c == '-' || c == '_':
			class = 0 // in the alphabet, but no class of its own
		default:
			flush(i)
			continue
		}
		if start < 0 {
			start = i
		}
		classes |= class
	}
	flush(len(text))
	return len(text) - entropy, entropy
}
