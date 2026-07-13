package interview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// PoolJudge grades via the agent VerifierPool (peer review). Defined consumer-side;
// satisfied by an adapter over VerifierPool + VerifyRequester at the composition
// root. A nil PoolJudge or an error (e.g. no eligible verifier) routes the grade
// to the inline judge.
type PoolJudge interface {
	GradeViaPool(ctx context.Context, agentID, question, answer string) (domain.VerifyResponse, error)
}

// HybridJudge realizes the chosen grading policy: use the agent VerifierPool when
// it can serve, otherwise an inline kernel LLM judge. This is bootstrap-safe by
// construction — at first boot no agent is trusted, so the pool returns
// ErrNoVerifierAvailable and the kernel judges; as the pool matures, peer review
// takes over automatically with no flag flip.
type HybridJudge struct {
	Pool   PoolJudge       // optional — nil or error ⇒ inline
	Inline *InlineLLMJudge // required for the bootstrap path
}

// Grade tries the pool first and falls back to the inline judge on any pool error.
func (h HybridJudge) Grade(ctx context.Context, agentID, question, answer string) (domain.VerifyResponse, error) {
	if h.Pool != nil {
		if resp, err := h.Pool.GradeViaPool(ctx, agentID, question, answer); err == nil {
			return resp, nil
		}
	}
	if h.Inline == nil {
		return domain.VerifyResponse{}, errors.New("interview judge: no inline judge configured")
	}
	return h.Inline.Grade(ctx, agentID, question, answer)
}

// InlineLLMJudge grades a (question, answer) with the kernel's own LLM — no agent
// dependency, so it works during bootstrap before any verifier is trusted.
type InlineLLMJudge struct {
	Gen Generator
}

// Grade asks the LLM for a 0–1 quality score and a short critique.
func (j *InlineLLMJudge) Grade(ctx context.Context, _ /*agentID*/, question, answer string) (domain.VerifyResponse, error) {
	if j == nil || j.Gen == nil {
		return domain.VerifyResponse{}, errors.New("inline judge: no generator")
	}
	raw, err := j.Gen.Generate(ctx, buildJudgePrompt(question, answer))
	if err != nil {
		return domain.VerifyResponse{}, err
	}
	score, critique := parseJudgement(raw)
	return domain.VerifyResponse{QualityScore: float32(score), Critique: critique}, nil
}

func buildJudgePrompt(question, answer string) string {
	// Canonical PROMPTREQ builder with a JSON output schema (see question_generator.go
	// for why format="json" matters on the managed Ollama path).
	schema := `{"type":"object","properties":{"score":{"type":"number","minimum":0,"maximum":1},"critique":{"type":"string"}},"required":["score","critique"]}`
	return domain.PromptBuild(
		domain.PromptSystem(
			"You are Cambrian's interview grader.",
			"Judge how well the answer actually accomplishes the task it was given.",
			"score: 0.0 = useless or wrong, 1.0 = excellent. critique: one sentence.",
		),
		domain.PromptContext(fmt.Sprintf("<Question>%s</Question>\n<Answer>%s</Answer>", question, answer)),
		domain.PromptTask("Grade the answer to the question."),
		domain.PromptOutputSchemaJSON(schema),
	)
}

var scoreRe = regexp.MustCompile(`(?i)score\s*[:=]?\s*([01](?:\.\d+)?|0?\.\d+)`)

// parseJudgement extracts a [0,1] score and a critique. It first tries the JSON
// contract ({"score":..,"critique":..}); if that fails it falls back to a `SCORE:`
// regex over free text. An unparseable score defaults to 0 (fail-low — an
// ungradeable answer earns no trust) rather than a misleading neutral.
func parseJudgement(raw string) (float64, string) {
	if score, critique, ok := parseJudgementJSON(raw); ok {
		return score, critique
	}
	text := strings.TrimSpace(raw)
	score := 0.0
	if m := scoreRe.FindStringSubmatch(text); m != nil {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			score = clamp(v, 0, 1)
		}
	}
	var critiqueLines []string
	for _, line := range strings.Split(text, "\n") {
		l := strings.TrimSpace(line)
		if l == "" || scoreRe.MatchString(l) {
			continue
		}
		critiqueLines = append(critiqueLines, l)
	}
	return score, strings.Join(critiqueLines, " ")
}

func parseJudgementJSON(raw string) (float64, string, bool) {
	start, end := strings.Index(raw, "{"), strings.LastIndex(raw, "}")
	if start < 0 || end <= start {
		return 0, "", false
	}
	var obj struct {
		Score    float64 `json:"score"`
		Critique string  `json:"critique"`
	}
	if err := json.Unmarshal([]byte(raw[start:end+1]), &obj); err != nil {
		return 0, "", false
	}
	return clamp(obj.Score, 0, 1), obj.Critique, true
}
