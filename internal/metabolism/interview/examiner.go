package interview

import (
	"context"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// The examiner turns the Provisional→Active interview into a real capability
// probe: an LLM generates questions from the agent's declaration, the agent
// ANSWERS them for real (not a self-assessed bid), and each answer is graded.
// The mean grade becomes the agent's cold-start routing prior — the signal the
// EFE selector and the Gatekeeper merit score were previously missing.
//
// All collaborators are consumer-side interfaces (hexagonal): the concrete LLM
// gateway, the Auctioneer's CallAgent, the VerifierPool, and the belief store are
// adapted to them at the composition root.

// Generator is the minimal LLM text surface the examiner needs (question
// generation + the inline judge). The kernel's LLM gateway / Planner satisfies it.
type Generator interface {
	Generate(ctx context.Context, prompt string) (string, error)
}

// ScenarioRunner executes one interview question against the agent for real and
// returns the agent's answer — the gradeable capability signal. Backed by the
// Auctioneer/Manager CallAgent at the composition root.
type ScenarioRunner interface {
	RunScenario(ctx context.Context, agent domain.AgentDefinition, question string, deadline time.Time) (answer string, latencyMs int, err error)
}

// Judge grades a (question, answer) pair on [0,1] with a critique. Implemented by
// HybridJudge: the agent VerifierPool when it can serve, else an inline kernel
// LLM judge (bootstrap-safe).
type Judge interface {
	Grade(ctx context.Context, agentID, question, answer string) (domain.VerifyResponse, error)
}

// QuestionGenerator produces the interview questions for an agent.
type QuestionGenerator interface {
	Questions(ctx context.Context, agent domain.AgentDefinition, manifest *domain.AgentManifest) []string
}

// BeliefSeeder seeds the EFE CapabilityBelief from a graded interview outcome.
// Optional (the belief store is not yet live — ADR-0037 deferred); the composition
// root adapts the VerifierConsolidator/belief store to this when promoted.
type BeliefSeeder interface {
	SeedFromInterview(ctx context.Context, agentID string, transcriptEmbedding []float32, score float64) error
}

// InterviewResult aggregates a graded interview.
type InterviewResult struct {
	MeanScore float64
	Questions []string
	Answers   []string
	Scores    []float64
	Critiques []string
}

// Transcript is the combined Q/A text used to embed the agent's profile vector —
// a far richer capability representation than the templated-scenario embedding it
// replaces (the agent's actual answers, not "how would you handle tool X?").
func (r InterviewResult) Transcript() string {
	var b []byte
	for i, q := range r.Questions {
		b = append(b, "Q: "...)
		b = append(b, q...)
		b = append(b, '\n')
		if i < len(r.Answers) {
			b = append(b, "A: "...)
			b = append(b, r.Answers[i]...)
			b = append(b, '\n')
		}
	}
	return string(b)
}

// Examiner drives the LLM interview: generate questions → execute each against
// the agent → grade → aggregate. A deep module over four injected seams. Any
// per-question failure degrades to a zero score for that question (a non-answer
// is a real, low-capability signal) rather than aborting the interview.
type Examiner struct {
	Questions QuestionGenerator
	Runner    ScenarioRunner
	Judge     Judge
	// PerScenarioTimeout bounds a single question's execute+grade. Zero (the
	// default) means NO deadline — the agent's LLM call is uncapped (a slow local
	// model is bounded only by the model client's own HTTP timeout). A tight
	// deadline here cascades into the agent as its GenerateViaModelStream budget,
	// so leaving it 0 is what keeps interview LLM calls from being killed early.
	PerScenarioTimeout time.Duration
}

// Run conducts the interview and returns the aggregate result. With no questions
// (or no collaborators) it returns a zero-question result and a 0 mean score.
func (e *Examiner) Run(ctx context.Context, agent domain.AgentDefinition, manifest *domain.AgentManifest) InterviewResult {
	qs := e.Questions.Questions(ctx, agent, manifest)
	res := InterviewResult{Questions: qs}
	if len(qs) == 0 {
		return res
	}

	var sum float64
	for _, q := range qs {
		// Zero deadline ⇒ no per-scenario cap (the agent's LLM call runs to
		// completion). Only set one when an explicit timeout is configured.
		var deadline time.Time
		if e.PerScenarioTimeout > 0 {
			deadline = time.Now().Add(e.PerScenarioTimeout)
		}
		answer, _, err := e.Runner.RunScenario(ctx, agent, q, deadline)
		if err != nil {
			res.Answers = append(res.Answers, "")
			res.Scores = append(res.Scores, 0)
			res.Critiques = append(res.Critiques, "no answer: "+err.Error())
			continue
		}
		var score float64
		var critique string
		grade, gerr := e.Judge.Grade(ctx, agent.ID, q, answer)
		if gerr == nil {
			score = clamp(float64(grade.QualityScore), 0, 1)
			critique = grade.Critique
		} else {
			critique = "ungraded: " + gerr.Error()
		}
		res.Answers = append(res.Answers, answer)
		res.Scores = append(res.Scores, score)
		res.Critiques = append(res.Critiques, critique)
		sum += score
	}
	res.MeanScore = sum / float64(len(qs))
	return res
}
