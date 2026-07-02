package awareness

import (
	"testing"
)

// ── Cycle 1: Single well-formed <thought> block + plan JSON ──────────────────

func TestParseThoughts_SingleBlock(t *testing.T) {
	raw := `<thought>I should search for music first.</thought>{"steps":[],"subject":"Music"}`

	thoughts, planJSON := ParseThoughts(raw)

	if len(thoughts) != 1 {
		t.Fatalf("expected 1 thought, got %d", len(thoughts))
	}
	if thoughts[0] != "I should search for music first." {
		t.Errorf("thought[0] = %q, want %q", thoughts[0], "I should search for music first.")
	}
	want := `{"steps":[],"subject":"Music"}`
	if planJSON != want {
		t.Errorf("planJSON = %q, want %q", planJSON, want)
	}
}

// ── Cycle 2: Multiple <thought> blocks + plan JSON ───────────────────────────

func TestParseThoughts_MultipleBlocks(t *testing.T) {
	raw := `<thought>First thought.</thought><thought>Second thought.</thought>{"steps":[],"subject":"Test"}`

	thoughts, planJSON := ParseThoughts(raw)

	if len(thoughts) != 2 {
		t.Fatalf("expected 2 thoughts, got %d", len(thoughts))
	}
	if thoughts[0] != "First thought." {
		t.Errorf("thought[0] = %q, want %q", thoughts[0], "First thought.")
	}
	if thoughts[1] != "Second thought." {
		t.Errorf("thought[1] = %q, want %q", thoughts[1], "Second thought.")
	}
	want := `{"steps":[],"subject":"Test"}`
	if planJSON != want {
		t.Errorf("planJSON = %q, want %q", planJSON, want)
	}
}

// ── Cycle 3: No <thought> blocks → empty thoughts, full input as planJSON ────

func TestParseThoughts_NoBlocks(t *testing.T) {
	raw := `{"steps":[],"subject":"NoThoughts"}`

	thoughts, planJSON := ParseThoughts(raw)

	if len(thoughts) != 0 {
		t.Fatalf("expected 0 thoughts, got %d: %v", len(thoughts), thoughts)
	}
	if planJSON != raw {
		t.Errorf("planJSON = %q, want %q", planJSON, raw)
	}
}

// ── Cycle 4: Empty input → empty thoughts, empty planJSON ────────────────────

func TestParseThoughts_EmptyInput(t *testing.T) {
	thoughts, planJSON := ParseThoughts("")

	if len(thoughts) != 0 {
		t.Fatalf("expected 0 thoughts, got %d", len(thoughts))
	}
	if planJSON != "" {
		t.Errorf("planJSON = %q, want empty string", planJSON)
	}
}

// ── Cycle 5: Unclosed <thought> tag → WARN logged, remainder as planJSON ─────

func TestParseThoughts_UnclosedTag(t *testing.T) {
	raw := `<thought>This thought is never closed... {"steps":[]}`

	thoughts, planJSON := ParseThoughts(raw)

	if len(thoughts) != 0 {
		t.Errorf("expected 0 thoughts for unclosed tag, got %d: %v", len(thoughts), thoughts)
	}
	if planJSON == "" {
		t.Error("planJSON must not be empty when unclosed tag leaves trailing text")
	}
}

// ── Cycle 6: <thought> block with surrounding whitespace → thought trimmed ───

func TestParseThoughts_WhitespaceTrimming(t *testing.T) {
	raw := "<thought>  \n  inner thought text  \n  </thought>  {\"steps\":[]}"

	thoughts, planJSON := ParseThoughts(raw)

	if len(thoughts) != 1 {
		t.Fatalf("expected 1 thought, got %d", len(thoughts))
	}
	if thoughts[0] != "inner thought text" {
		t.Errorf("thought[0] = %q, want %q", thoughts[0], "inner thought text")
	}
	want := `{"steps":[]}`
	if planJSON != want {
		t.Errorf("planJSON = %q, want %q", planJSON, want)
	}
}

// ── Cycle 7: planJSON is trimmed (no leading/trailing whitespace) ─────────────

func TestParseThoughts_PlanJSONTrimmed(t *testing.T) {
	raw := "  \n  {\"steps\":[]}\n  "

	thoughts, planJSON := ParseThoughts(raw)

	if len(thoughts) != 0 {
		t.Fatalf("expected 0 thoughts, got %d", len(thoughts))
	}
	want := `{"steps":[]}`
	if planJSON != want {
		t.Errorf("planJSON = %q, want %q", planJSON, want)
	}
}
