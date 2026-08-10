package app

import "strings"

// chatFailureNote turns a failed turn into one short line for the person waiting on it.
//
// Why this exists: a turn that fails before replying delivers nothing. On 2026-08-07 a chat
// turn logged "Sending full prompt to LLM" and then produced silence for four minutes,
// while the provider had already answered "Weekly usage limit reached. Resets in 2 days" in
// half a second. The operator saw a message sent and no reply — indistinguishable from a
// hang, and impossible to act on. The classification below is the difference between "it is
// broken" and "it is out of quota until Sunday, go top it up".
//
// It deliberately CLASSIFIES rather than forwarding err.Error(). These notes ride the
// ADR-0098 progress channel, whose Text() contract is explicit that the string "reaches end
// users on surfaces we do not control the formatting of" — Telegram among them — and so
// "carries no markup, no internal identifiers, and no punctuation that a transport might
// interpret". A raw provider error fails all three: they carry workspace ids, billing URLs
// and JSON. The full detail is already in the log (planner.go logs the error, and the LLM
// transport logs the provider's own message on a quota refusal), which is where an operator
// should read it — not from a line that may be relayed to a stranger's phone.
func chatFailureNote(err error) string {
	if err == nil {
		return ""
	}
	low := strings.ToLower(err.Error())

	// Ordered most-specific first: a quota refusal often ALSO surfaces as a deadline once
	// the retry ladder has burned the caller's budget, and reporting that as a timeout
	// sends the operator looking at latency instead of at their bill.
	switch {
	case containsAny(low, "usage limit", "usagelimit", "quota", "credit balance", "billing", "exceeded your current"):
		return "the language model provider refused this request because its usage limit is reached"
	case containsAny(low, "unauthorized", "invalid api key", "authentication", "401", "403"):
		return "the language model provider rejected our credentials"
	case containsAny(low, "no such model", "model not found", "unknown model"):
		return "the configured language model is not available from its provider"
	case containsAny(low, "deadline exceeded", "context deadline", "timeout", "timed out"):
		return "the language model did not answer in time"
	case containsAny(low, "connection refused", "no such host", "unreachable", "dial tcp", "eof"):
		return "the language model provider could not be reached"
	case containsAny(low, "circuit", "unhealthy", "no healthy"):
		return "the language model provider is temporarily unavailable"
	default:
		// Deliberately vague rather than leaking an internal error string to a surface
		// we do not control. The log has the specifics.
		return "this turn could not be completed, the kernel log has the detail"
	}
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}
