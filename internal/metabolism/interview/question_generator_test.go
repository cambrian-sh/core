package interview

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// The managed Ollama path forces JSON, so the primary contract is a JSON object.
func TestLLMQuestionGenerator_ParsesJSON(t *testing.T) {
	gen := fakeGen{out: `{"questions": ["What is X?", "How do you Y?", "Explain Z"]}`}
	g := LLMQuestionGenerator{Gen: gen, Count: 3}
	qs := g.Questions(context.Background(), domain.AgentDefinition{ID: "a1"}, &domain.AgentManifest{Tools: []string{"x"}})
	if len(qs) != 3 || qs[0] != "What is X?" {
		t.Fatalf("JSON questions not parsed: %v", qs)
	}
}

// The question prompt is assembled with the canonical PROMPTREQ builder, so it
// carries the XML envelope + a JSON OutputSchema (not an ad-hoc text instruction).
func TestQuestionPrompt_UsesCanonicalBuilder(t *testing.T) {
	p := buildQuestionPrompt("a web research agent", &domain.AgentManifest{Tools: []string{"web_search"}}, 3)
	for _, want := range []string{"<System>", "<Role>", "<Task>", `<OutputSchema format="json">`, "web_search"} {
		if !strings.Contains(p, want) {
			t.Errorf("question prompt missing %q:\n%s", want, p)
		}
	}
}

// Non-JSON providers still work via the line-parsing fallback (numbering stripped).
func TestLLMQuestionGenerator_ParsesAndStripsNumbering(t *testing.T) {
	gen := fakeGen{out: "1. What is X?\n2) How do you Y?\n- Explain Z"}
	g := LLMQuestionGenerator{Gen: gen, Count: 3}
	qs := g.Questions(context.Background(), domain.AgentDefinition{ID: "a1"}, &domain.AgentManifest{Tools: []string{"x"}})
	if len(qs) != 3 {
		t.Fatalf("want 3 questions, got %d: %v", len(qs), qs)
	}
	for _, q := range qs {
		if strings.HasPrefix(q, "1.") || strings.HasPrefix(q, "-") || strings.HasPrefix(q, "2)") {
			t.Errorf("numbering/markers not stripped: %q", q)
		}
	}
	if qs[0] != "What is X?" {
		t.Errorf("first question = %q", qs[0])
	}
}

func TestLLMQuestionGenerator_RespectsCount(t *testing.T) {
	gen := fakeGen{out: "a\nb\nc\nd\ne"}
	g := LLMQuestionGenerator{Gen: gen, Count: 2}
	qs := g.Questions(context.Background(), domain.AgentDefinition{}, &domain.AgentManifest{})
	if len(qs) != 2 {
		t.Errorf("Count=2 must cap questions, got %d", len(qs))
	}
}

// No generator ⇒ fall back to the static scenario templates (never blocks).
func TestLLMQuestionGenerator_FallsBackWhenNoGenerator(t *testing.T) {
	g := LLMQuestionGenerator{Gen: nil}
	qs := g.Questions(context.Background(), domain.AgentDefinition{}, &domain.AgentManifest{Tools: []string{"sql"}})
	if len(qs) == 0 {
		t.Fatal("expected template fallback questions, got none")
	}
}

// LLM error ⇒ template fallback.
func TestLLMQuestionGenerator_FallsBackOnError(t *testing.T) {
	g := LLMQuestionGenerator{Gen: fakeGen{err: errors.New("llm down")}}
	qs := g.Questions(context.Background(), domain.AgentDefinition{}, &domain.AgentManifest{Tools: []string{"sql"}})
	if len(qs) == 0 {
		t.Fatal("expected template fallback on LLM error, got none")
	}
}

// Empty LLM output ⇒ template fallback (never zero questions when tools exist).
func TestLLMQuestionGenerator_FallsBackOnEmptyOutput(t *testing.T) {
	g := LLMQuestionGenerator{Gen: fakeGen{out: "   \n  \n"}}
	qs := g.Questions(context.Background(), domain.AgentDefinition{}, &domain.AgentManifest{Tools: []string{"sql"}})
	if len(qs) == 0 {
		t.Fatal("blank LLM output must fall back to templates")
	}
}
