package interview

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// DefaultQuestionCount is how many interview questions to generate when unset.
const DefaultQuestionCount = 3

// LLMQuestionGenerator generates interview questions with an LLM from the agent's
// declared capability (its description + manifest tools). It falls back to the
// static scenario templates when the LLM is unavailable or returns nothing, so
// the interview never blocks on the model (graceful degradation).
type LLMQuestionGenerator struct {
	Gen   Generator
	Count int
}

// Questions returns up to Count LLM-generated interview questions, or the
// template scenarios on any failure.
func (g LLMQuestionGenerator) Questions(ctx context.Context, agent domain.AgentDefinition, manifest *domain.AgentManifest) []string {
	n := g.Count
	if n <= 0 {
		n = DefaultQuestionCount
	}
	if g.Gen == nil {
		return buildScenarios(manifest)
	}
	raw, err := g.Gen.Generate(ctx, buildQuestionPrompt(agent.Description, manifest, n))
	if err != nil {
		return buildScenarios(manifest)
	}
	qs := parseQuestions(raw, n)
	if len(qs) == 0 {
		return buildScenarios(manifest)
	}
	return qs
}

func buildQuestionPrompt(description string, manifest *domain.AgentManifest, n int) string {
	tools := "(none declared)"
	if manifest != nil && len(manifest.Tools) > 0 {
		tools = strings.Join(manifest.Tools, ", ")
	}
	if description == "" {
		description = "(no description provided)"
	}
	// Canonical PROMPTREQ builder: <System>/<Context>/<Task>/<OutputSchema format="json">.
	// The format="json" tag is what the structured-output layer (and the managed
	// Ollama Format:"json") expects — a plain-text instruction would make the model
	// emit a confused JSON error instead of questions.
	schema := fmt.Sprintf(
		`{"type":"object","properties":{"questions":{"type":"array","items":{"type":"string"},"minItems":%d,"maxItems":%d}},"required":["questions"]}`,
		n, n,
	)
	return domain.PromptBuild(
		domain.PromptSystem(
			"You are Cambrian's agent interviewer.",
			"Assess an agent's REAL capability before admitting it to the multi-agent system.",
			fmt.Sprintf("Write %d concrete, answerable task questions that reveal whether the agent can do what it claims.", n),
			"Prefer specific tasks over trivia. Base the questions ONLY on the declared profile below.",
		),
		domain.PromptContext(fmt.Sprintf(
			"<AgentProfile>\n  <Description>%s</Description>\n  <Tools>%s</Tools>\n</AgentProfile>",
			description, tools,
		)),
		domain.PromptTask(fmt.Sprintf("Generate exactly %d interview questions for this agent.", n)),
		domain.PromptOutputSchemaJSON(schema),
	)
}

// parseQuestions extracts up to n questions. It first tries the JSON contract
// ({"questions":[...]}); if that fails (a provider not in JSON mode) it falls back
// to one-question-per-line parsing with list markers stripped.
func parseQuestions(raw string, n int) []string {
	if qs := parseQuestionsJSON(raw); len(qs) > 0 {
		return capQuestions(qs, n)
	}
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		q := strings.TrimSpace(line)
		q = strings.TrimLeft(q, "-*0123456789.) \t")
		q = strings.TrimSpace(q)
		if q == "" || strings.HasPrefix(q, "{") || strings.HasPrefix(q, "}") {
			continue
		}
		out = append(out, q)
		if len(out) >= n {
			break
		}
	}
	return out
}

func parseQuestionsJSON(raw string) []string {
	start, end := strings.Index(raw, "{"), strings.LastIndex(raw, "}")
	if start < 0 || end <= start {
		return nil
	}
	var obj struct {
		Questions []string `json:"questions"`
	}
	if err := json.Unmarshal([]byte(raw[start:end+1]), &obj); err != nil {
		return nil
	}
	var out []string
	for _, q := range obj.Questions {
		if s := strings.TrimSpace(q); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func capQuestions(qs []string, n int) []string {
	if n > 0 && len(qs) > n {
		return qs[:n]
	}
	return qs
}
