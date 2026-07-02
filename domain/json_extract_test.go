package domain

import "testing"

func TestStripReasoning(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"no wrapper", "hello", "hello"},
		{"closed think", "<think>reasoning</think>answer", "answer"},
		{"case-insensitive", "<THINK>x</THINK>answer", "answer"},
		{"thinking variant", "<thinking>x</thinking>answer", "answer"},
		{"multiline dotall", "<think>line1\nline2</think>answer", "answer"},
		{"dangling unclosed", "before<think>ran out of tokens", "before"},
		{"multiple blocks", "<think>a</think>mid<think>b</think>end", "midend"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := StripReasoning(c.in); got != c.want {
				t.Errorf("StripReasoning(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestExtractJSONObject(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"plain object", `{"a":1}`, `{"a":1}`},
		{"prose preamble", `Here is the plan: {"a":1}`, `{"a":1}`},
		{"trailing text", `{"a":1} done`, `{"a":1}`},
		// The qwen3 failure: a <think> block with stray braces precedes the object.
		{"reasoning wrapper with stray braces",
			`<think>I'll emit {like this} maybe</think>{"steps":[1,2]}`,
			`{"steps":[1,2]}`},
		// An object whose body contains an array must be returned WHOLE, not the
		// inner array — this is why the planner needs object semantics, not array.
		{"object containing array", `{"steps":[1,2,3]}`, `{"steps":[1,2,3]}`},
		// A stray '{' that doesn't start a valid value is skipped for the real one.
		{"stray brace then real object", `{ not json {"a":1}`, `{"a":1}`},
		{"no object", `just prose`, ""},
		{"only dangling think", `<think>never finished`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ExtractJSONObject(c.in); got != c.want {
				t.Errorf("ExtractJSONObject(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestExtractJSONArray(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"plain array", `[1,2,3]`, `[1,2,3]`},
		// The Tier-2 scorer failure mode: stray brackets inside the think block.
		{"reasoning wrapper with stray brackets",
			`<think>scores like [{...}] for sure</think>[{"tier":"FULL"}]`,
			`[{"tier":"FULL"}]`},
		{"array-first preference over leading object",
			`{"note":"x"} [1,2]`, `[1,2]`},
		{"fallback to object when no array", `{"a":1}`, `{"a":1}`},
		{"none", `prose only`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ExtractJSONArray(c.in); got != c.want {
				t.Errorf("ExtractJSONArray(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
