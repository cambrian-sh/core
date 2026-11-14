package domain

import "testing"

// The inputs the three former copies (llm extractJSON, awareness stripCodeFence,
// router decodeClassification) all handled identically.
func TestStripCodeFence(t *testing.T) {
	for in, want := range map[string]string{
		`{"a":1}`:                    `{"a":1}`,
		"  {\"a\":1}\n":              `{"a":1}`,
		"```json\n{\"a\":1}\n```":    `{"a":1}`,
		"```\n{\"a\":1}\n```":        `{"a":1}`,
		" \n```json\n{\"a\":1}```\n": `{"a":1}`,
		"```json\n{\"a\":1}":         `{"a":1}`, // unclosed fence: the opening is still dropped
		"prose ```json\n{}```":       "prose ```json\n{}```",
		"```":                        "",
	} {
		if got := StripCodeFence(in); got != want {
			t.Errorf("StripCodeFence(%q) = %q, want %q", in, got, want)
		}
	}
}
