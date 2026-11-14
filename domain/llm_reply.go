package domain

import "strings"

// StripCodeFence is the one tolerance for a structured reply a model wrapped in a
// markdown code fence: it trims the text, and when it starts with ``` drops that
// opening fence with its language-tag line ("```json") and a closing ``` — nothing
// else (no prose stripping, no second fence). Text that does not start with a fence
// is returned trimmed and otherwise unchanged.
//
// Callers decide WHEN it applies: a reply whose schema the provider enforced needs
// no tolerance (GenerateResponse.SchemaUnchecked reports the other case). The rule
// used to be written out three times — llm/schema.go extractJSON,
// awareness/organ_request.go stripCodeFence and router/default_router.go
// decodeClassification — and copies drift
// (T-20261105-code-fence-decode-tolerance-for-unchecked).
func StripCodeFence(text string) string {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "```") {
		return t
	}
	t = strings.TrimPrefix(t, "```")
	if nl := strings.IndexByte(t, '\n'); nl >= 0 {
		t = t[nl+1:] // the language tag line ("json")
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(t), "```"))
}
