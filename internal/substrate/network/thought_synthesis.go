package network

import (
	"fmt"
	"strings"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/prompts"
)

// Thought-step synthesis (ADR-0139 C1/C4/D2).
//
// A thought step is the kernel answering a plan step itself from the results of the
// steps it depends on. Its request is two library prompts: thought_synthesis, the
// stable system block (cached), and thought_synthesis_input, the user message carrying
// the goal, the task and the results as tagged data. The results are the executor's
// domain.StepDependency manifest (handoff metadata domain.HandoffKeyStepDependencies):
// in DependsOn order, already bounded by execution.plan.dependency_results_max_chars,
// each with its step index, objective, status and content — the same structure an SDK
// agent step receives, so a dependent step sees the same upstream results whichever
// executor runs it (T-20261105-a-planner-thought-step-that-depends-on-two).

const (
	thoughtSystemPromptID = "thought_synthesis"
	thoughtInputPromptID  = "thought_synthesis_input"
)

// thoughtDataTags are the data regions of thought_synthesis_input.
var thoughtDataTags = []string{"goal", "task", "results", "result", "objective", "content", "error"}

// thoughtNoResults fills the results region of a thought step that depends on no
// earlier step, so the model is told there is nothing upstream rather than shown an
// empty region it could read as "the results went missing".
const thoughtNoResults = "(none: this step depends on no earlier step)"

// dependencyResultsMaxChars resolves execution.plan.dependency_results_max_chars:
// <= 0 reads as the config default.
func dependencyResultsMaxChars(v int) int {
	if v <= 0 {
		return config.DefaultDependencyResultsMaxChars
	}
	return v
}

// thoughtGenerator is the generator a thought step is served by: ThoughtLLM (the
// thought-purpose generator, a C1 RequestGenerator whose calls reach hooks and the
// spend meter labelled "thought") when wired, else the Planner's legacy Generate,
// which domain.GenerateVia serves as one flattened prompt.
func (s *Server) thoughtGenerator() domain.Generator {
	if s.ThoughtLLM != nil {
		return s.ThoughtLLM
	}
	return s.Planner
}

// thoughtResults reads the step's upstream results from its handoff. A malformed
// manifest is an error, not "no results": synthesising without the inputs the step
// depends on is exactly the failure this path exists to prevent.
func thoughtResults(handoff *domain.Handoff) ([]domain.StepDependency, error) {
	if handoff == nil {
		return nil, nil
	}
	deps, err := domain.DecodeStepDependencies(handoff.Context[domain.HandoffKeyStepDependencies])
	if err != nil {
		return nil, fmt.Errorf("thought step: %w", err)
	}
	return deps, nil
}

// renderThoughtResults renders the results region: one <result> per upstream step.
func renderThoughtResults(deps []domain.StepDependency) string {
	if len(deps) == 0 {
		return thoughtNoResults
	}
	esc := func(v string) string { return prompts.NeutraliseClosingTags(v, thoughtDataTags...) }
	var rb strings.Builder
	for i, d := range deps {
		if i > 0 {
			rb.WriteByte('\n')
		}
		if d.PriorPlan > 0 {
			// A result carried forward from a plan a replan swapped out: its step
			// number indexes that plan, so it is labelled apart from this plan's steps.
			fmt.Fprintf(&rb, "<result step=\"prior_step_%d\" from=\"previous_plan_%d\" status=%q>\n<objective>%s</objective>\n",
				d.Step, d.PriorPlan, d.Status, esc(strings.TrimSpace(d.Objective)))
		} else {
			fmt.Fprintf(&rb, "<result step=\"%d\" status=%q>\n<objective>%s</objective>\n",
				d.Step, d.Status, esc(strings.TrimSpace(d.Objective)))
		}
		switch {
		case d.Status == domain.StepDependencyFailed:
			fmt.Fprintf(&rb, "<error>%s</error>\n", esc(d.Error))
		case d.Status != domain.StepDependencyCompleted:
			// Nothing to show; the status says why.
		case d.Shortened:
			ref := "no content id, the rest cannot be retrieved"
			if d.CID != "" {
				ref = "full result cid " + d.CID
			}
			fmt.Fprintf(&rb, "<content>\n%s\n[shortened: %d of %d characters shown; %s]\n</content>\n",
				esc(d.Content), len(d.Content), d.Bytes, ref)
		default:
			fmt.Fprintf(&rb, "<content>\n%s\n</content>\n", esc(d.Content))
		}
		rb.WriteString("</result>")
	}
	return rb.String()
}

// buildThoughtRequest renders one thought step's synthesis call.
func buildThoughtRequest(goal, task string, deps []domain.StepDependency) (domain.GenerateRequest, error) {
	sys, err := prompts.Get(thoughtSystemPromptID)
	if err != nil {
		return domain.GenerateRequest{}, fmt.Errorf("thought step: %w", err)
	}
	in, err := prompts.Get(thoughtInputPromptID)
	if err != nil {
		return domain.GenerateRequest{}, fmt.Errorf("thought step: %w", err)
	}
	msg, err := in.Render(map[string]string{
		"goal":    prompts.NeutraliseClosingTags(strings.TrimSpace(goal), thoughtDataTags...),
		"task":    prompts.NeutraliseClosingTags(strings.TrimSpace(task), thoughtDataTags...),
		"results": renderThoughtResults(deps),
	})
	if err != nil {
		return domain.GenerateRequest{}, fmt.Errorf("thought step: %w", err)
	}
	return domain.GenerateRequest{
		System:   []domain.SystemBlock{{Text: sys.Body, Cache: true}},
		Messages: []domain.ModelMessage{domain.UserMessage(msg.Text)},
	}, nil
}
