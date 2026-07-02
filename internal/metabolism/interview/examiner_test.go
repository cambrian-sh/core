package interview

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// --- fakes ---

type fakeQuestionGen struct{ qs []string }

func (f fakeQuestionGen) Questions(_ context.Context, _ domain.AgentDefinition, _ *domain.AgentManifest) []string {
	return f.qs
}

// fakeRunner returns a canned answer per question, or an error for questions in errOn.
type fakeRunner struct {
	answers map[string]string
	errOn   map[string]bool
}

func (f fakeRunner) RunScenario(_ context.Context, _ domain.AgentDefinition, q string, _ time.Time) (string, int, error) {
	if f.errOn[q] {
		return "", 0, errors.New("agent unreachable")
	}
	return f.answers[q], 10, nil
}

// fakeJudge grades each answer by a fixed map; an unknown answer scores 0.
type fakeJudge struct{ scores map[string]float64 }

func (f fakeJudge) Grade(_ context.Context, _, _, answer string) (domain.VerifyResponse, error) {
	return domain.VerifyResponse{QualityScore: float32(f.scores[answer]), Critique: "ok"}, nil
}

func agentDef() domain.AgentDefinition { return domain.AgentDefinition{ID: "a1", Description: "does things"} }

// The examiner runs every question, grades each answer, and reports the MEAN —
// the cold-start prior. Answers preserve order alongside their questions.
func TestExaminer_AggregatesMeanGrade(t *testing.T) {
	ex := &Examiner{
		Questions: fakeQuestionGen{qs: []string{"q1", "q2"}},
		Runner:    fakeRunner{answers: map[string]string{"q1": "good", "q2": "bad"}},
		Judge:     fakeJudge{scores: map[string]float64{"good": 1.0, "bad": 0.0}},
	}
	res := ex.Run(context.Background(), agentDef(), &domain.AgentManifest{})

	if len(res.Questions) != 2 || len(res.Answers) != 2 || len(res.Scores) != 2 {
		t.Fatalf("expected 2 of each, got q=%d a=%d s=%d", len(res.Questions), len(res.Answers), len(res.Scores))
	}
	if res.MeanScore != 0.5 {
		t.Errorf("MeanScore = %v, want 0.5 (mean of 1.0 and 0.0)", res.MeanScore)
	}
	if res.Answers[0] != "good" || res.Answers[1] != "bad" {
		t.Errorf("answers out of order: %v", res.Answers)
	}
}

// A question the agent cannot answer scores 0 (a real low-capability signal) and
// does NOT abort the interview — the other questions still count.
func TestExaminer_NonAnswerScoresZeroNotAbort(t *testing.T) {
	ex := &Examiner{
		Questions: fakeQuestionGen{qs: []string{"q1", "q2"}},
		Runner: fakeRunner{
			answers: map[string]string{"q2": "great"},
			errOn:   map[string]bool{"q1": true},
		},
		Judge: fakeJudge{scores: map[string]float64{"great": 1.0}},
	}
	res := ex.Run(context.Background(), agentDef(), &domain.AgentManifest{})

	if res.MeanScore != 0.5 {
		t.Errorf("MeanScore = %v, want 0.5 (0 for the unanswered + 1.0)", res.MeanScore)
	}
	if res.Scores[0] != 0 {
		t.Errorf("unanswered question must score 0, got %v", res.Scores[0])
	}
}

// No questions ⇒ zero result, no panic, mean 0.
func TestExaminer_NoQuestions(t *testing.T) {
	ex := &Examiner{
		Questions: fakeQuestionGen{qs: nil},
		Runner:    fakeRunner{},
		Judge:     fakeJudge{},
	}
	res := ex.Run(context.Background(), agentDef(), &domain.AgentManifest{})
	if res.MeanScore != 0 || len(res.Questions) != 0 {
		t.Errorf("empty interview must be zero-valued, got mean=%v n=%d", res.MeanScore, len(res.Questions))
	}
}

// The transcript interleaves Q and A — the richer profile-embedding source.
func TestInterviewResult_Transcript(t *testing.T) {
	res := InterviewResult{Questions: []string{"q1"}, Answers: []string{"a1"}}
	got := res.Transcript()
	if got != "Q: q1\nA: a1\n" {
		t.Errorf("Transcript = %q", got)
	}
}
