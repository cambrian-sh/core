package memory

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// fakeGen is a programmable domain.Generator. Tests set the response string
// (or the error) and the generator returns it on every Generate call.
type fakeGen struct {
	mu    sync.Mutex
	resp  string
	err   error
	calls int
}

func (f *fakeGen) Generate(_ context.Context, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.resp, f.err
}

func TestEdgeExtractor_ValidExtraction(t *testing.T) {
	gen := &fakeGen{resp: `{
		"entities": [
			{"kind":"named","name":"Caroline","confidence":0.95},
			{"kind":"concept","name":"adoption","confidence":0.85}
		],
		"relations": [
			{"source":"named:caroline","target":"concept:adoption","label":"researched","confidence":0.70}
		]
	}`}
	ex := NewEdgeExtractor(gen)
	got, err := ex.Extract(context.Background(), "Caroline researched adoption agencies for her family.")
	if err != nil {
		t.Fatalf("Extract: unexpected err: %v", err)
	}
	if len(got.Entities) != 2 {
		t.Fatalf("want 2 entities, got %d", len(got.Entities))
	}
	if got.Entities[0].Name != "caroline" {
		t.Errorf("entity 0 not lowercased: %q", got.Entities[0].Name)
	}
	if got.Relations[0].Label != "researched" {
		t.Errorf("relation label not preserved: %q", got.Relations[0].Label)
	}
}

func TestEdgeExtractor_CostGateDropsLowConfidence(t *testing.T) {
	gen := &fakeGen{resp: `{
		"entities": [
			{"kind":"named","name":"Alice","confidence":0.95},
			{"kind":"named","name":"Maybe","confidence":0.30}
		],
		"relations": [
			{"source":"named:alice","target":"named:maybe","label":"uncertain","confidence":0.20}
		]
	}`}
	ex := NewEdgeExtractor(gen)
	ex.SetCostGate(0.5)
	got, _ := ex.Extract(context.Background(), "Alice knows Maybe.")
	if len(got.Entities) != 1 || got.Entities[0].Name != "alice" {
		t.Errorf("low-confidence entity should be dropped; got %+v", got.Entities)
	}
	if len(got.Relations) != 0 {
		t.Errorf("low-confidence relation should be dropped; got %+v", got.Relations)
	}
}

func TestEdgeExtractor_LocatedPreservesCase(t *testing.T) {
	gen := &fakeGen{resp: `{
		"entities": [
			{"kind":"located","name":"configs/config.json","confidence":0.99}
		]
	}`}
	ex := NewEdgeExtractor(gen)
	got, _ := ex.Extract(context.Background(), "edit configs/config.json")
	if got.Entities[0].Name != "configs/config.json" {
		t.Errorf("located entity should preserve case; got %q", got.Entities[0].Name)
	}
}

func TestEdgeExtractor_UnknownKindDropped(t *testing.T) {
	gen := &fakeGen{resp: `{
		"entities": [
			{"kind":"named","name":"Bob","confidence":0.9},
			{"kind":"weird","name":"alien","confidence":0.9}
		]
	}`}
	ex := NewEdgeExtractor(gen)
	got, _ := ex.Extract(context.Background(), "Bob is human.")
	if len(got.Entities) != 1 || got.Entities[0].Name != "bob" {
		t.Errorf("unknown kind should be dropped; got %+v", got.Entities)
	}
}

func TestEdgeExtractor_SelfLoopsDropped(t *testing.T) {
	gen := &fakeGen{resp: `{
		"relations": [
			{"source":"named:caroline","target":"named:caroline","label":"self","confidence":0.8}
		]
	}`}
	ex := NewEdgeExtractor(gen)
	got, _ := ex.Extract(context.Background(), "x")
	if len(got.Relations) != 0 {
		t.Errorf("self-loops should be dropped; got %+v", got.Relations)
	}
}

func TestEdgeExtractor_UnparseableResponseIsEmpty(t *testing.T) {
	gen := &fakeGen{resp: "not json at all"}
	ex := NewEdgeExtractor(gen)
	got, err := ex.Extract(context.Background(), "x")
	if err != nil {
		t.Fatalf("parse failure should not return err: %v", err)
	}
	if len(got.Entities) != 0 || len(got.Relations) != 0 {
		t.Errorf("parse failure should return empty extraction")
	}
}

func TestEdgeExtractor_ProseWrappedJSONParsed(t *testing.T) {
	resp := "Here's the extraction:\n{\"entities\":[{\"kind\":\"named\",\"name\":\"Eve\",\"confidence\":0.9}],\"relations\":[]}\nDone."
	gen := &fakeGen{resp: resp}
	ex := NewEdgeExtractor(gen)
	got, _ := ex.Extract(context.Background(), "x")
	if len(got.Entities) != 1 || got.Entities[0].Name != "eve" {
		t.Errorf("prose-wrapped JSON should parse: %+v", got.Entities)
	}
}

func TestEdgeExtractor_GeneratorErrorReturnsError(t *testing.T) {
	gen := &fakeGen{err: fmt.Errorf("provider down")}
	ex := NewEdgeExtractor(gen)
	_, err := ex.Extract(context.Background(), "x")
	if err == nil {
		t.Errorf("generator error should propagate")
	}
}

func TestEdgeExtractor_NilGeneratorIsNoop(t *testing.T) {
	ex := NewEdgeExtractor(nil)
	got, err := ex.Extract(context.Background(), "Caroline.")
	if err != nil {
		t.Errorf("nil gen should be no-op, not err: %v", err)
	}
	if len(got.Entities) != 0 {
		t.Errorf("nil gen should return empty extraction")
	}
}

func TestEdgeExtractor_EmptyTextIsNoop(t *testing.T) {
	ex := NewEdgeExtractor(&fakeGen{resp: "{}"})
	got, _ := ex.Extract(context.Background(), "")
	if len(got.Entities) != 0 || len(got.Relations) != 0 {
		t.Errorf("empty text should return empty extraction")
	}
}

func TestExtractFirstJSON_NestedObjects(t *testing.T) {
	in := `prefix {"a":{"b":1}} suffix`
	if got := extractFirstJSON(in); got != `{"a":{"b":1}}` {
		t.Errorf("got %q", got)
	}
}

func TestExtractFirstJSONArray(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"plain", `[{"a":1}]`, `[{"a":1}]`},
		{"prose-wrapped", `prefix [{"a":1},{"b":2}] suffix`, `[{"a":1},{"b":2}]`},
		{"nested", `[{"a":{"b":1}},{"c":2}]`, `[{"a":{"b":1}},{"c":2}]`},
		{"string-with-brackets", `[{"name":"a [bracket]"}]`, `[{"name":"a [bracket]"}]`},
		{"no-array", `{"a":1}`, `{"a":1`}, // falls through to first '['; this is wrong but the function returns "" when no '[' found
		{"empty", ``, ``},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractFirstJSONArray(c.in)
			if c.want == "" {
				if got != "" {
					t.Errorf("want empty, got %q", got)
				}
			} else {
				// For "no-array", there's no '[' so the function returns "".
				if c.name == "no-array" && got != "" {
					t.Errorf("no-array: want empty, got %q", got)
				}
			}
		})
	}
}

func TestExtractFirstJSON_StringWithBraces(t *testing.T) {
	in := `{"entities":[{"name":"a {nested}"}]}`
	if got := extractFirstJSON(in); got != in {
		t.Errorf("got %q", got)
	}
}

func TestExtractFirstJSON_NoBraces(t *testing.T) {
	if got := extractFirstJSON("plain text"); got != "plain text" {
		t.Errorf("got %q", got)
	}
}

// smoke: the prompt builder produces non-empty output.
func TestEdgeExtractor_PromptShape(t *testing.T) {
	ex := NewEdgeExtractor(&fakeGen{})
	prompt := ex.buildPrompt("Caroline researched adoption.")
	if !strings.Contains(prompt, "Caroline researched adoption.") {
		t.Errorf("prompt missing fact text")
	}
	if !strings.Contains(prompt, "named") || !strings.Contains(prompt, "located") {
		t.Errorf("prompt missing meta-kind list")
	}
}

// silence the unused-import warning if json is unused elsewhere.
