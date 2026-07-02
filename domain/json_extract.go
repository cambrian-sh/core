package domain

import (
	"encoding/json"
	"regexp"
	"strings"
)

// LLM responses from reasoning models (e.g. qwen3) wrap their answer in a
// chain-of-thought block before the intended JSON payload. That block frequently
// contains stray '[' / '{' characters that derail naive bracket-scanning, and
// the real JSON always comes AFTER it. These helpers strip the reasoning wrapper
// and extract the first COMPLETE JSON value, so a reasoning model's output parses
// through the same path as a plain model's. Centralized here (pure, stdlib-only,
// zero external imports) so every LLM consumer shares one hardened extractor
// instead of drifting copies — see internal/memory (Tier-2 scorer, array) and
// internal/awareness (planner/replan, object).

// reasoningBlock matches a closed chain-of-thought wrapper. (?is) = dotall +
// case-insensitive; non-greedy so multiple blocks each match minimally.
var reasoningBlock = regexp.MustCompile(`(?is)<think(?:ing)?>.*?</think(?:ing)?>`)

// danglingReasoning strips an UNCLOSED reasoning block — the model spent its
// token budget thinking and never emitted a closing tag or the answer — leaving
// no JSON to find (callers then degrade gracefully).
var danglingReasoning = regexp.MustCompile(`(?is)<think(?:ing)?>.*$`)

// StripReasoning removes <think>/<thinking> chain-of-thought wrappers (closed and
// dangling-unclosed) from an LLM response.
func StripReasoning(s string) string {
	s = reasoningBlock.ReplaceAllString(s, "")
	s = danglingReasoning.ReplaceAllString(s, "")
	return s
}

// ExtractJSONObject strips reasoning wrappers and returns the first complete JSON
// object. Use for responses expected to be a single object (e.g. an ExecutionPlan).
func ExtractJSONObject(s string) string {
	return firstCompleteJSON(StripReasoning(s), '{')
}

// ExtractJSONArray strips reasoning wrappers and returns the first complete JSON
// array, falling back to the first complete object. Use for responses expected to
// be an array (e.g. the Tier-2 scorer's per-result score list).
func ExtractJSONArray(s string) string {
	s = StripReasoning(s)
	if arr := firstCompleteJSON(s, '['); arr != "" {
		return arr
	}
	return firstCompleteJSON(s, '{')
}

// firstCompleteJSON returns the first substring starting at byte `open` that
// decodes to a complete JSON value, scanning left-to-right. A stray bracket that
// does not begin a valid value is skipped. "" if none.
func firstCompleteJSON(s string, open byte) string {
	for i := 0; i < len(s); i++ {
		if s[i] != open {
			continue
		}
		var raw json.RawMessage
		if err := json.NewDecoder(strings.NewReader(s[i:])).Decode(&raw); err == nil {
			return string(raw)
		}
	}
	return ""
}
