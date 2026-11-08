package domain

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// testVocabulary mirrors the shipped default's shape (config.DefaultEffectVocabulary)
// without importing config (domain imports nothing internal).
func testVocabulary(t *testing.T) *StepEffectVocabulary {
	t.Helper()
	v, err := NewStepEffectVocabulary([]StepEffectTerm{
		{Name: "read", Classes: []ToolEffect{EffectRead}},
		{Name: "write", Classes: []ToolEffect{EffectWrite}},
		{Name: "move", Refines: EffectWrite, Tools: []string{"*move*", "*rename*"}},
		{Name: "delete", Refines: EffectWrite, Tools: []string{"*delete*"}},
		{Name: "egress", Classes: []ToolEffect{EffectEgress}},
		{Name: "send", Refines: EffectEgress, Tools: []string{"*send*"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestStepEffects_DerivedFromToolPolicies(t *testing.T) {
	v := testVocabulary(t)
	tools := []SystemTool{
		{Name: "mcp:filesystem/read_text_file", Effects: []ToolEffect{EffectRead}},
		{Name: "mcp:filesystem/move_file", Effects: []ToolEffect{EffectWrite}},
		{Name: "mcp:gmail/send_email", Effects: []ToolEffect{EffectRead, EffectEgress}},
	}
	got := v.DeriveEffects(tools)
	want := []string{"egress", "move", "read", "send", "write"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("derived = %v, want %v", got, want)
	}
}

// A tool with no operator policy contributes nothing: inferred effects and the
// fail-closed default of an unlisted MCP tool are not evidence of what it does.
func TestStepEffects_ToolWithoutPolicyContributesNothing(t *testing.T) {
	v := testVocabulary(t)
	for name, tool := range map[string]SystemTool{
		"no effects":        {Name: "mcp:x/move_file"},
		"inferred effects":  {Name: "mcp:x/move_file", Effects: []ToolEffect{EffectRead, EffectWrite}, EffectsInferred: true},
		"defaulted effects": {Name: "mcp:x/move_file", Effects: []ToolEffect{EffectRead, EffectWrite, EffectEgress}, EffectsDefaulted: true},
	} {
		if got := v.EffectsOfTool(tool); len(got) != 0 {
			t.Errorf("%s: contributed %v, want nothing", name, got)
		}
	}
}

// A refined term needs BOTH the class and the name: write_file is not a move, and a
// read-only tool named "move_*" is not one either.
func TestStepEffects_RefinedTermNeedsClassAndName(t *testing.T) {
	v := testVocabulary(t)
	if got := v.EffectsOfTool(SystemTool{Name: "mcp:filesystem/write_file", Effects: []ToolEffect{EffectWrite}}); !reflect.DeepEqual(got, []string{"write"}) {
		t.Errorf("write_file = %v, want [write]", got)
	}
	if got := v.EffectsOfTool(SystemTool{Name: "mcp:x/move_preview", Effects: []ToolEffect{EffectRead}}); !reflect.DeepEqual(got, []string{"read"}) {
		t.Errorf("read-only move_preview = %v, want [read]", got)
	}
	// The glob matches the base name only: a server id never matches.
	if got := v.EffectsOfTool(SystemTool{Name: "mcp:mover/write_file", Effects: []ToolEffect{EffectWrite}}); !reflect.DeepEqual(got, []string{"write"}) {
		t.Errorf("server id matched a refinement glob: %v", got)
	}
}

func TestStepEffects_UnknownEffectIsAlwaysMissing(t *testing.T) {
	v := testVocabulary(t)
	have := []string{"read", "write", "move"}
	if got := v.MissingEffects([]string{"teleport"}, have); !reflect.DeepEqual(got, []string{"teleport"}) {
		t.Errorf("missing = %v, want [teleport]", got)
	}
	// Even an agent that "has" the unknown name cannot satisfy it.
	if got := v.MissingEffects([]string{"teleport"}, []string{"teleport"}); len(got) != 1 {
		t.Errorf("an unknown effect was satisfied: %v", got)
	}
	if got := v.MissingEffects([]string{" Move "}, have); len(got) != 0 {
		t.Errorf("case/space normalisation failed: %v", got)
	}
	var nilVocab *StepEffectVocabulary
	if got := nilVocab.MissingEffects([]string{"read"}, have); len(got) != 1 {
		t.Errorf("a nil vocabulary must leave every effect unmatched, got %v", got)
	}
}

func TestStepEffectVocabulary_Validation(t *testing.T) {
	bad := map[string][]StepEffectTerm{
		"empty name":      {{Name: "", Classes: []ToolEffect{EffectRead}}},
		"uppercase":       {{Name: "Move", Refines: EffectWrite, Tools: []string{"*move*"}}},
		"duplicate":       {{Name: "read", Classes: []ToolEffect{EffectRead}}, {Name: "read", Classes: []ToolEffect{EffectRead}}},
		"unknown class":   {{Name: "x", Classes: []ToolEffect{"teleport"}}},
		"no derivation":   {{Name: "x"}},
		"refine no tools": {{Name: "x", Refines: EffectWrite}},
		"refine bad":      {{Name: "x", Refines: "teleport", Tools: []string{"*"}}},
		"both":            {{Name: "x", Classes: []ToolEffect{EffectRead}, Refines: EffectWrite, Tools: []string{"*"}}},
	}
	for name, terms := range bad {
		if _, err := NewStepEffectVocabulary(terms); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if v, err := NewStepEffectVocabulary(nil); err != nil || len(v.Names()) != 0 {
		t.Errorf("an empty vocabulary is valid (fail closed): %v", err)
	}
}

func TestNoEffectMatchError_IsADispatchRefusal(t *testing.T) {
	e := &NoEffectMatchError{TaskID: "task-1", Missing: []string{"move"}, Check: &EffectCheckResult{
		Required: []string{"move"}, Outcome: EffectCheckRefused,
		AgentEffects: map[string][]string{"researcher": {"egress", "read"}},
	}}
	wrapped := fmt.Errorf("step: %w", e)
	if !IsDispatchRefusal(wrapped) {
		t.Fatal("a wrapped NoEffectMatchError must be a dispatch refusal")
	}
	if IsDispatchRefusal(errors.New("agent crashed")) {
		t.Fatal("an ordinary error is not a refusal")
	}
	msg := e.Error()
	for _, want := range []string{"no agent can move", "researcher=[egress,read]", "required [move]"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q lacks %q", msg, want)
		}
	}
}
