package awareness

import (
	"context"
	"strings"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

type fakeGen struct {
	gotPrompt string
	resp      string
}

func (f *fakeGen) Generate(_ context.Context, prompt string) (string, error) {
	f.gotPrompt = prompt
	return f.resp, nil
}

// The validator returns a structured verdict from the LLM response and instructs
// a verdict-only answer (no echo of the step output) — fixing the gate that
// previously parroted its input and grepped for a magic token.
func TestLLMCheckpointValidator_StructuredVerdict(t *testing.T) {
	gen := &fakeGen{resp: "VERDICT: INCOHERENT\nThe recurrence is wrong."}
	v := NewLLMCheckpointValidator(gen)

	verdict, err := v.Validate(context.Background(), domain.CheckpointRequest{
		StepQuery:  "Analyse the time complexity",
		StepOutput: "It is O(n log log n) because ...",
		Question:   "Is the analysis correct?",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if verdict.Coherent {
		t.Error("Coherent = true, want false (LLM said INCOHERENT)")
	}

	// The prompt must demand a verdict-only answer and forbid echoing the output.
	p := strings.ToUpper(gen.gotPrompt)
	if !strings.Contains(p, "VERDICT:") {
		t.Error("prompt should instruct the VERDICT: format")
	}
	if !strings.Contains(p, "DO NOT") && !strings.Contains(p, "DON'T") {
		t.Error("prompt should forbid echoing the step output")
	}
}

func TestLLMCheckpointValidator_CoherentPasses(t *testing.T) {
	gen := &fakeGen{resp: "VERDICT: COHERENT — the analysis is correct and justified."}
	v := NewLLMCheckpointValidator(gen)
	verdict, err := v.Validate(context.Background(), domain.CheckpointRequest{StepQuery: "q", StepOutput: "o"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !verdict.Coherent {
		t.Error("Coherent = false, want true")
	}
}

var _ domain.CheckpointValidator = (*LLMCheckpointValidator)(nil)
