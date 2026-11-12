package executer

import (
	"context"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// Step-output spans (ADR-0145 D10, ADR-0139 C6 run event log): every executed plan
// step is recorded as an "invoke_agent {agent}" span carrying the step as
// dispatched (query, dependencies, required effects) and the output it handed on,
// so a reader can see what each dependant received. A start event (same span id,
// "cambrian.phase": "start") is written when the step is dispatched. Model and tool calls inside
// the step are their own spans (llm/run_trace_layer.go, network/execute_tool.go);
// all share the session's trace id.

// executeStepTraced is executeStep plus its span; the span is skipped when no
// tracer is wired.
func (d *DAGExecutor) executeStepTraced(
	ctx context.Context,
	plan *domain.ExecutionPlan,
	stepIndex int,
	snapshot map[string]string,
	workingMemory []domain.ContextRef,
	deps stepDeps,
	stepFn StepFunc,
	planID string,
) stepResult {
	if d.RunTracer == nil {
		return d.executeStep(ctx, plan, stepIndex, snapshot, workingMemory, deps, stepFn, planID)
	}
	// The start event names the step as dispatched, so a follower sees which step
	// is running before its agent answers (the agent is known only at the end).
	id, start := domain.StartTraceSpan(ctx, d.RunTracer, domain.TraceSpan{
		Name: domain.GenAIOpInvokeAgent + " " + stepAgentPlaceholder(plan.Steps[stepIndex]), Kind: "INTERNAL",
		Attributes: stepAttrs(plan, stepIndex, planID, d.CurrentRunID)})
	res := d.executeStep(ctx, plan, stepIndex, snapshot, workingMemory, deps, stepFn, planID)
	s := stepSpan(plan, stepIndex, planID, d.CurrentRunID, start, res)
	s.SpanID = id
	d.RunTracer.Span(ctx, s)
	return res
}

// stepAgentPlaceholder names a step's span before its agent is known.
func stepAgentPlaceholder(step domain.Step) string {
	if step.IsThought {
		return "thought"
	}
	return "step"
}

// stepAttrs are the step as dispatched: on the start event and the finished span.
func stepAttrs(plan *domain.ExecutionPlan, stepIndex int, planID string, runID domain.RunID) map[string]any {
	step := plan.Steps[stepIndex]
	attrs := map[string]any{
		domain.GenAIOperationName:  domain.GenAIOpInvokeAgent,
		"cambrian.plan.id":         planID,
		"cambrian.plan.subject":    plan.Subject,
		"cambrian.run.id":          string(runID),
		"cambrian.step.index":      stepIndex,
		"cambrian.step.task_id":    formatTaskID(stepIndex, planID),
		"cambrian.step.query":      step.Query,
		"cambrian.step.depends_on": step.DependsOn,
		"cambrian.step.is_thought": step.IsThought,
	}
	if step.RequiredEffects != nil {
		attrs["cambrian.step.required_effects"] = step.RequiredEffects
	}
	return attrs
}

func stepSpan(plan *domain.ExecutionPlan, stepIndex int, planID string, runID domain.RunID, start time.Time, res stepResult) domain.TraceSpan {
	agent := stepAgentPlaceholder(plan.Steps[stepIndex])
	attrs := stepAttrs(plan, stepIndex, planID, runID)
	attrs["cambrian.step.cached"] = res.cached
	if res.resp != nil {
		if res.resp.FromAgent != "" {
			agent = res.resp.FromAgent
			attrs[domain.GenAIAgentName] = res.resp.FromAgent
		}
		var text string
		if res.resp.Payload != nil {
			text = string(res.resp.Payload.Data)
			attrs["cambrian.step.output_type"] = res.resp.Payload.Type
		}
		attrs[domain.GenAIOutputMessages] = []map[string]any{domain.GenAITextMessage("assistant", text)}
		attrs[domain.GenAIUsageInputTokens] = res.promptTokens
		attrs[domain.GenAIUsageOutputTokens] = res.completionTokens
	}
	s := domain.TraceSpan{Name: domain.GenAIOpInvokeAgent + " " + agent, Kind: "INTERNAL",
		Start: start, End: time.Now(), Attributes: attrs}
	if res.err != nil {
		s.Error = res.err.Error()
	}
	return s
}
