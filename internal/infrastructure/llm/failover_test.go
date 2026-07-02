package llm

import (
	"errors"
	"testing"
)

// healthySetExcept returns a predicate where every id is healthy except those listed.
func healthySetExcept(open ...string) func(string) bool {
	openSet := make(map[string]bool)
	for _, id := range open {
		openSet[id] = true
	}
	return func(id string) bool { return !openSet[id] }
}

func TestResolveModel_SuggestedHealthyShortCircuits(t *testing.T) {
	got, err := resolveModel("deepseek", nil, []string{"qwen-local"}, []string{"deepseek", "qwen-local"}, "qwen-local", healthySetExcept(), nil)
	if err != nil || got != "deepseek" {
		t.Fatalf("want deepseek, got %q (%v)", got, err)
	}
}

func TestResolveModel_SuggestedOpenFallsToPreference(t *testing.T) {
	got, err := resolveModel("deepseek", nil, []string{"qwen-local", "gpt4o"}, []string{"deepseek", "qwen-local", "gpt4o"}, "qwen-local", healthySetExcept("deepseek"), nil)
	if err != nil || got != "qwen-local" {
		t.Fatalf("want qwen-local (first healthy preference), got %q (%v)", got, err)
	}
}

func TestResolveModel_PreferenceAndDefaultOpenFallsToCapabilityMatch(t *testing.T) {
	capIndex := map[string][]string{"code": {"gpt4o"}}
	got, err := resolveModel(
		"deepseek", []string{"code"},
		[]string{"qwen-local"}, // preference all open
		[]string{"deepseek", "qwen-local", "gpt4o"},
		"qwen-local", // default open
		healthySetExcept("deepseek", "qwen-local"),
		capIndex,
	)
	if err != nil || got != "gpt4o" {
		t.Fatalf("want gpt4o (capability match), got %q (%v)", got, err)
	}
}

func TestResolveModel_NoneHealthyReturnsTypedError(t *testing.T) {
	_, err := resolveModel("deepseek", nil, []string{"qwen-local"}, []string{"deepseek", "qwen-local"}, "qwen-local", healthySetExcept("deepseek", "qwen-local"), nil)
	if !errors.Is(err, ErrNoHealthyModel) {
		t.Fatalf("want ErrNoHealthyModel, got %v", err)
	}
}

func TestResolveModel_EmptyHintsCapabilityRungIsAnyHealthy(t *testing.T) {
	// suggested, preference, default all open; no hints => capability rung
	// considers all ids, picks first healthy (gpt4o).
	got, err := resolveModel(
		"deepseek", nil,
		[]string{"qwen-local"},
		[]string{"deepseek", "qwen-local", "gpt4o"},
		"qwen-local",
		healthySetExcept("deepseek", "qwen-local"),
		nil,
	)
	if err != nil || got != "gpt4o" {
		t.Fatalf("want gpt4o (any healthy), got %q (%v)", got, err)
	}
}

func TestResolveModel_CapabilityRequiresAllHints(t *testing.T) {
	// gpt4o has only "code"; deepseek has both. Hints {code,vision} => only an id
	// advertising both qualifies.
	capIndex := map[string][]string{"code": {"gpt4o", "deepseek"}, "vision": {"deepseek"}}
	got, err := resolveModel(
		"", []string{"code", "vision"},
		nil,
		[]string{"gpt4o", "deepseek"},
		"", // no default
		healthySetExcept(),
		capIndex,
	)
	if err != nil || got != "deepseek" {
		t.Fatalf("want deepseek (has all hints), got %q (%v)", got, err)
	}
}
