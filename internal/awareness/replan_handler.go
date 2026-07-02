package awareness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// ── ReplanHandler prompt constants ───────────────────────────────────────────

const replanRole = "You are the Cambrian Replanning Engine. The current plan has partially failed and you must produce a revised plan for the REMAINING work."

const replanStaticRules = `STRICT RULES:
- Do NOT re-include steps that already succeeded (they are listed above in <Context>).
- The new plan's step 0 corresponds to the first incomplete step of the original plan.
- Each step must have "query" (natural language) and "depends_on" (int array).
- The subject should be "Replan: <original subject>".`

// replanStaticText is hashed for the registry. Dynamic fields (failed step,
// error, partial results, budget/checkpoint blocks) are excluded from the hash.
const replanStaticText = replanRole + replanStaticRules + PlanOutputSchema

var replanPromptHash = domain.PromptHashOf(replanStaticText)

func init() {
	domain.PromptRegistry[replanPromptHash] = domain.PromptEntry{
		ID:      "planner.replan",
		Version: "1.0.0",
		Hash:    replanPromptHash,
		Schema:  PlanOutputSchema,
	}
}

// ── End ReplanHandler prompt constants ───────────────────────────────────────

// PlannerReplanHandler wraps the Planner to implement substrate.ReplanHandler.
// It builds a specialised replan prompt with error context and partial results,
// calls the Planner's Generate method, and parses the response into a new
// ExecutionPlan for the remaining work.
type PlannerReplanHandler struct {
	planner Generator
}

// NewPlannerReplanHandler creates a replan handler backed by any Generator.
func NewPlannerReplanHandler(planner Generator) *PlannerReplanHandler {
	return &PlannerReplanHandler{planner: planner}
}

// Replan calls the Planner with error context and partial results to produce
// a revised ExecutionPlan for the remaining work. Returns nil if replanning
// fails or if the Planner cannot produce a valid plan.
func (h *PlannerReplanHandler) Replan(
	ctx context.Context,
	failedStep int,
	err error,
	partialContext map[string]string,
	originalPlan *domain.ExecutionPlan,
) (*domain.ExecutionPlan, error) {
	errMsg := err.Error()
	failedStepQuery := ""
	if failedStep >= 0 && failedStep < len(originalPlan.Steps) {
		failedStepQuery = originalPlan.Steps[failedStep].Query
	}

	var partialResults strings.Builder
	stepCount := 0
	for k, v := range partialContext {
		if strings.HasPrefix(k, "step_") && strings.HasSuffix(k, "_result") {
			partialResults.WriteString(fmt.Sprintf("- %s: %s\n", k, v))
			stepCount++
		}
	}

	modelConstraint := ""
	var budgetErr *domain.BudgetExceededError
	if errors.As(err, &budgetErr) {
		modelConstraint = `
BUDGET CONSTRAINT:
- The plan exceeded its cost budget. You MUST use ONLY the cheapest available model for ALL remaining steps.
- Override any "recommended_model" with the cheapest model available.
- Every remaining step should prefer thought steps (is_thought: true) over agent steps where possible, to avoid costly external agent invocations.

`
	}

	var checkpointErr *domain.SemanticCheckpointError
	if errors.As(err, &checkpointErr) {
		checkpointQuery := ""
		if checkpointErr.StepIndex >= 0 && checkpointErr.StepIndex < len(originalPlan.Steps) {
			checkpointQuery = originalPlan.Steps[checkpointErr.StepIndex].CheckpointQuery
		}
		modelConstraint = fmt.Sprintf(
			`
CHECKPOINT FAILURE:
- Step %d completed successfully but its output was assessed as incoherent before
  downstream steps were dispatched.
- Checkpoint assessment: %s
- Original coherence question: "%s"
- Do NOT simply retry the failed step. Diagnose why the output failed the coherence
  check and redesign the approach for the remaining work.

`,
			checkpointErr.StepIndex,
			checkpointErr.Assessment,
			checkpointQuery,
		)
	}

	// Build the dynamic context string: original request + failure details.
	avoidConstraint := fmt.Sprintf("- Avoid the tool/agent approach that failed: %s", failedStepQuery)
	contextContent := fmt.Sprintf(
		"Original request: %s\n\nFailed step: step %d — %s\nError: %s\n%s\nPartial results so far (%d steps completed):\n%s",
		originalPlan.Subject, failedStep, failedStepQuery, errMsg, modelConstraint, stepCount, partialResults.String(),
	)

	prompt := domain.PromptBuild(
		domain.PromptSystem(replanRole, replanStaticRules, avoidConstraint),
		domain.PromptContext(contextContent),
		domain.PromptTask(fmt.Sprintf("Produce a revised ExecutionPlan in JSON for the REMAINING work only. Subject: \"Replan: %s\".", originalPlan.Subject)),
		domain.PromptOutputSchemaJSON(PlanOutputSchema),
	)

	slog.Info("🔄 Replanning", "failed_step", failedStep, "error", errMsg)

	responseStr, genErr := h.planner.Generate(ctx, prompt)
	if genErr != nil {
		slog.Error("ReplanHandler: Planner.Generate failed", "error", genErr)
		return nil, genErr
	}

	match := domain.ExtractJSONObject(responseStr)
	if match == "" {
		slog.Error("ReplanHandler: no JSON object found in Planner response")
		return nil, fmt.Errorf("replan: no JSON found in response")
	}

	var plan domain.ExecutionPlan
	if unmarshalErr := json.Unmarshal([]byte(match), &plan); unmarshalErr != nil {
		slog.Error("ReplanHandler: failed to parse replan JSON", "error", unmarshalErr)
		return nil, fmt.Errorf("replan: parse error: %w", unmarshalErr)
	}

	// PROMPTREQ: record the replan prompt hash so PlanEvent.PlannerPromptVersion
	// reflects the last accepted Generate() call.
	plan.PlannerPromptVersion = replanPromptHash

	slog.Info("✅ Replan produced", "steps", len(plan.Steps), "subject", plan.Subject)
	return &plan, nil
}
