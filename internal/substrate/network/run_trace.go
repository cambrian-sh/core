package network

import (
	"context"
	"strings"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// traceToolCall records one kernel-executed tool call as an "execute_tool {name}"
// span (ADR-0145 D10, OTel GenAI execute-tool.md): the arguments exactly as the
// model sent them and the result as returned to the agent (bounded by the tracer),
// or the content-store reference when the result was offloaded. Tools an agent
// runs locally are traced by the SDK (cambrian_agent_sdk/run_trace.py).
func (s *Server) traceToolCall(ctx context.Context, start time.Time, agentID string, lease domain.LeaseID, req *pb.ExecuteToolRequest, resp domain.ToolCallResponse) {
	if s.RunTracer == nil {
		return
	}
	attrs := map[string]any{
		domain.GenAIOperationName:     domain.GenAIOpExecuteTool,
		domain.GenAIToolName:          req.GetToolName(),
		domain.GenAIToolCallArguments: req.GetArgsJson(),
		domain.GenAIToolCallResult:    string(resp.ResultJSON),
		"cambrian.lease.id":           string(lease),
		"cambrian.tool.denied":        resp.Denied,
	}
	if agentID != "" {
		attrs[domain.GenAIAgentName] = agentID
	}
	if tid := mdValue(ctx, "x-task-id"); tid != "" {
		attrs["cambrian.step.task_id"] = tid
	}
	if resp.ResultCID != "" {
		attrs["cambrian.tool.result_cid"] = resp.ResultCID
	}
	if resp.DenyReason != "" {
		attrs["cambrian.tool.deny_reason"] = resp.DenyReason
		// ADR-0145 D14.5: answered from the step's refusal record, not run again.
		attrs["cambrian.tool.repeated_refusal"] = strings.HasSuffix(resp.DenyReason, domain.RepeatedRefusalNote)
	}
	if resp.ApprovalState != "" {
		attrs["cambrian.tool.approval_state"] = string(resp.ApprovalState)
	}
	sp := domain.TraceSpan{Name: domain.GenAIOpExecuteTool + " " + req.GetToolName(), Kind: "INTERNAL",
		Start: start, End: time.Now(), Attributes: attrs}
	if resp.Error != "" {
		sp.Error = resp.Error
	}
	s.RunTracer.Span(ctx, sp)
}
