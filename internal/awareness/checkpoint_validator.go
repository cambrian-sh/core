package awareness

import (
	"context"
	"fmt"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// LLMCheckpointValidator is the H1 coherence gate (ADR-0013) implemented as a
// dedicated validator: it asks the LLM for a structured VERDICT and parses it
// deterministically. Unlike the old path (the synthesis ThoughtFn whose
// free-text output was grepped for a magic token), it demands a verdict-only
// answer with no echo of the step output and never rubber-stamps on a parse miss.
type LLMCheckpointValidator struct {
	gen domain.Generator
}

// NewLLMCheckpointValidator wires the validator to a text generator (which
// should be the Langfuse-wrapped generator so the check is traced).
func NewLLMCheckpointValidator(gen domain.Generator) *LLMCheckpointValidator {
	return &LLMCheckpointValidator{gen: gen}
}

// Validate assesses a step output against its intent and returns a structured
// verdict. A generator error fails open (coherent) so a transient LLM failure
// never fabricates a replan; the error is returned for the caller to log.
func (v *LLMCheckpointValidator) Validate(ctx context.Context, req domain.CheckpointRequest) (domain.CheckpointVerdict, error) {
	prompt := buildValidatorPrompt(req)
	raw, err := v.gen.Generate(ctx, prompt)
	if err != nil {
		return domain.CheckpointVerdict{Coherent: true, Assessment: "validator error; proceeding"}, err
	}
	return domain.ParseCheckpointVerdict(raw), nil
}

func buildValidatorPrompt(req domain.CheckpointRequest) string {
	question := req.Question
	if question == "" {
		question = "Is this output coherent and sufficient to satisfy the step query?"
	}
	return domain.PromptBuild(
		domain.PromptSystem(
			"You are the Cambrian Checkpoint Validator. The step output below is the acting agent's OWN account of executing the step — treat that account as the evidence of what happened (the agent reporting that it wrote a file, or that the work was already completed in an earlier step, IS evidence, not an invented fact).",
			"Judge ONLY whether that account coherently satisfies the step's intent. A clear, on-intent completion report is COHERENT — even when it references earlier steps or says no further action is needed. Mark INCOHERENT only when the output is empty, off-topic, self-contradictory, an error or refusal, or plainly does not satisfy the intent. Do not demand external proof, and do not invent facts of your own.",
			"Answer on a single first line as exactly `VERDICT: COHERENT` or `VERDICT: INCOHERENT`, optionally followed by one short sentence of reasoning.",
			"DO NOT repeat, quote, or echo the step output. Output only the verdict line (plus at most one sentence).",
		),
		domain.PromptTask(fmt.Sprintf(
			"Original Step Query: %q\nCheckpoint Question: %s\n\nStep Output:\n%s",
			req.StepQuery, question, req.StepOutput,
		)),
		domain.PromptOutputSchemaEnum(
			"VERDICT: COHERENT",
			"VERDICT: INCOHERENT",
		),
	)
}
