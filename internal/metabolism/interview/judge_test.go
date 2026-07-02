package interview

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// fakeGen returns canned text (or an error) for any prompt.
type fakeGen struct {
	out string
	err error
}

func (f fakeGen) Generate(_ context.Context, _ string) (string, error) { return f.out, f.err }

// fakePoolJudge simulates the agent VerifierPool: a canned score, or an error
// (e.g. ErrNoVerifierAvailable at bootstrap).
type fakePoolJudge struct {
	score float32
	err   error
}

func (f fakePoolJudge) GradeViaPool(_ context.Context, _, _, _ string) (domain.VerifyResponse, error) {
	if f.err != nil {
		return domain.VerifyResponse{}, f.err
	}
	return domain.VerifyResponse{QualityScore: f.score, Critique: "pool"}, nil
}

// When the pool can serve, it grades (peer review at maturity).
func TestHybridJudge_UsesPoolWhenAvailable(t *testing.T) {
	j := HybridJudge{
		Pool:   fakePoolJudge{score: 0.9},
		Inline: &InlineLLMJudge{Gen: fakeGen{out: "SCORE: 0.1"}},
	}
	resp, err := j.Grade(context.Background(), "a1", "q", "answer")
	if err != nil {
		t.Fatal(err)
	}
	if resp.QualityScore != 0.9 || resp.Critique != "pool" {
		t.Errorf("expected pool grade 0.9, got %v (%q)", resp.QualityScore, resp.Critique)
	}
}

// When the pool can't serve (bootstrap: no eligible verifier), fall back to the
// inline kernel judge — the bootstrap-safety guarantee.
func TestHybridJudge_FallsBackToInlineOnPoolError(t *testing.T) {
	j := HybridJudge{
		Pool:   fakePoolJudge{err: errors.New("no eligible verifier")},
		Inline: &InlineLLMJudge{Gen: fakeGen{out: "SCORE: 0.7\nReasonable answer."}},
	}
	resp, err := j.Grade(context.Background(), "a1", "q", "answer")
	if err != nil {
		t.Fatal(err)
	}
	if resp.QualityScore != 0.7 {
		t.Errorf("expected inline fallback grade 0.7, got %v", resp.QualityScore)
	}
}

// With no pool configured at all, the inline judge is used directly.
func TestHybridJudge_NoPoolUsesInline(t *testing.T) {
	j := HybridJudge{Inline: &InlineLLMJudge{Gen: fakeGen{out: "score = 0.5"}}}
	resp, err := j.Grade(context.Background(), "a1", "q", "answer")
	if err != nil {
		t.Fatal(err)
	}
	if resp.QualityScore != 0.5 {
		t.Errorf("expected 0.5, got %v", resp.QualityScore)
	}
}

// The judge prompt is assembled with the canonical PROMPTREQ builder + a JSON
// OutputSchema.
func TestJudgePrompt_UsesCanonicalBuilder(t *testing.T) {
	p := buildJudgePrompt("What is 2+2?", "4")
	for _, want := range []string{"<System>", "<Task>", `<OutputSchema format="json">`, "<Question>", "<Answer>"} {
		if !strings.Contains(p, want) {
			t.Errorf("judge prompt missing %q:\n%s", want, p)
		}
	}
}

func TestParseJudgement(t *testing.T) {
	cases := []struct {
		raw  string
		want float64
	}{
		// JSON contract (the forced-JSON path).
		{`{"score": 0.8, "critique": "Good but incomplete."}`, 0.8},
		{`{"score": 1.0, "critique": ""}`, 1.0},
		{`{"score": 2.5, "critique": "x"}`, 1.0}, // JSON number clamped to [0,1]
		// Free-text fallback (non-JSON providers).
		{"SCORE: 0.8\nGood but incomplete.", 0.8},
		{"score=1.0", 1.0},
		{"no score at all here", 0.0},
		{"SCORE: 2.5", 0.0}, // out-of-contract (>1) in regex path: rejected → fail-low
	}
	for _, c := range cases {
		score, _ := parseJudgement(c.raw)
		if score != c.want {
			t.Errorf("parseJudgement(%q) score = %v, want %v", c.raw, score, c.want)
		}
	}
}

// JSON judge output (the forced-JSON managed path) is parsed end-to-end.
func TestInlineJudge_ParsesJSON(t *testing.T) {
	j := &InlineLLMJudge{Gen: fakeGen{out: `{"score": 0.6, "critique": "decent"}`}}
	resp, err := j.Grade(context.Background(), "a1", "q", "answer")
	if err != nil {
		t.Fatal(err)
	}
	if resp.QualityScore != 0.6 || resp.Critique != "decent" {
		t.Errorf("JSON grade not parsed: %v %q", resp.QualityScore, resp.Critique)
	}
}

// An unparseable judge response fails LOW (0), never a misleading neutral.
func TestInlineJudge_UnparseableFailsLow(t *testing.T) {
	j := &InlineLLMJudge{Gen: fakeGen{out: "the answer seems fine to me"}}
	resp, err := j.Grade(context.Background(), "a1", "q", "answer")
	if err != nil {
		t.Fatal(err)
	}
	if resp.QualityScore != 0 {
		t.Errorf("unparseable grade must be 0, got %v", resp.QualityScore)
	}
}

func TestInlineJudge_GeneratorErrorPropagates(t *testing.T) {
	j := &InlineLLMJudge{Gen: fakeGen{err: errors.New("llm down")}}
	if _, err := j.Grade(context.Background(), "a1", "q", "answer"); err == nil {
		t.Error("expected generator error to propagate")
	}
}
