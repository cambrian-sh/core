package network

import (
	"context"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0145 D12: the agent-plane half of composed workers. The dispatcher
// (internal/metabolism/compose) binds a step's composition to its lease; here the
// kernel serves and enforces it:
//
//   - ListTools: the menu of a composed lease is its composition's tools (added
//     through the lease's run-scoped grants, then filtered by every ceiling, the
//     composition's own included, and by the step's effect menu).
//   - ListSkills: a composed lease's skill index is its composition's skills. A
//     skill outside it is never served and never opens an operator access request:
//     the model may narrow what the kernel composed, never widen it.
//   - ExecuteTool: enforced by the ToolExecutor's lease ceiling (ComposedLeases is
//     chained into it at the composition root), which holds under the unrestricted
//     bypass and over a loaded skill's grants.

// withComposedTools adds the composition's tools to a composed lease's menu, in the
// composition's (sorted) order after the agent's own; no-op for any other lease and
// for a describe_tool request (which names its tools and already sees the lease's
// run-scoped grants).
func (s *Server) withComposedTools(ctx context.Context, agentID, lease string, tools []domain.SystemTool) []domain.SystemTool {
	if s.Compositions == nil || s.ToolExecutor == nil || len(toolNamesFromMetadata(ctx)) > 0 {
		return tools
	}
	comp, ok := s.Compositions.For(lease)
	if !ok || len(comp.Tools) == 0 {
		return tools
	}
	have := make(map[string]bool, len(tools))
	for _, t := range tools {
		have[t.Name] = true
	}
	for _, t := range s.ToolExecutor.AvailableToolsNamedFor(ctx, agentID, lease, comp.Tools) {
		if !have[t.Name] {
			have[t.Name] = true
			tools = append(tools, t)
		}
	}
	return tools
}

// composedSkillAccess is the D12 skill bound: bound=false for a caller that is
// neither on a composed lease nor the composed runtime (the per-agent access rules
// apply), else whether a skill is in the composition's index (nothing, for the
// runtime without a composed lease).
func (s *Server) composedSkillAccess(ctx context.Context, agentID string) (inSet func(string) bool, bound bool) {
	comp, composed, runtimeOnly := s.composedFor(ctx, agentID)
	switch {
	case composed:
		return comp.HasSkill, true
	case runtimeOnly:
		return func(string) bool { return false }, true
	}
	return nil, false
}

// SkillAccessComposedOut is the access value ListSkills reports for a skill outside
// a composed worker's index: not served, and not requestable.
const SkillAccessComposedOut = domain.SkillAccessDenied

// callerLease is the lease the caller presented on this RPC: the lease header
// (tool calls, menus) or, for a skill load, x-session-token. "" when none, or when
// the lease is not bound to the caller.
func (s *Server) callerLease(ctx context.Context) string {
	if tok := s.callerOverlayToken(ctx); tok != "" {
		return tok
	}
	lease := domain.LeaseID(mdValue(ctx, "x-session-token"))
	if lease == "" || s.leaseForeignToCaller(ctx, lease) {
		return ""
	}
	return string(lease)
}

// composedFor resolves the caller's composition. runtimeOnly is true when the
// caller is the composed runtime but presented no composed lease: it then gets no
// tools and no skills at all (fail closed — the runtime holds nothing of its own,
// and under tools_unrestricted an uncomposed call would otherwise reach every tool).
func (s *Server) composedFor(ctx context.Context, agentID string) (comp domain.WorkerComposition, composed, runtimeOnly bool) {
	if s.Compositions != nil {
		if lease := s.callerLease(ctx); lease != "" {
			if c, ok := s.Compositions.For(lease); ok {
				return c, true, false
			}
		}
	}
	if s.ComposedRuntimeID != "" && agentID == s.ComposedRuntimeID {
		return domain.WorkerComposition{}, false, true
	}
	return domain.WorkerComposition{}, false, false
}

// composedToolRefusal is ExecuteTool's D12 refusal: the composed runtime calling
// without a composed lease. Every other bound is the executor's lease ceiling.
func (s *Server) composedToolRefusal(ctx context.Context, agentID string) (string, bool) {
	if _, _, runtimeOnly := s.composedFor(ctx, agentID); runtimeOnly {
		return "the composed worker runtime may call tools only under a composed step lease (ADR-0145 D12)", true
	}
	return "", false
}

// inheritComposition binds a D12 subagent child of a composed step to its parent's
// composition, narrowed to the child's toolset (the subagent ceiling bounds it to
// that set anyway; the composition keeps its skill index and lets the runtime act
// on the child lease at all). No-op for a parent that is not composed. The child's
// entry goes with its lease (the gateway's completion hook).
func (s *Server) inheritComposition(parent, child domain.LeaseID, toolset map[string]bool) {
	if s.Compositions == nil {
		return
	}
	comp, ok := s.Compositions.For(string(parent))
	if !ok {
		return
	}
	var kept []string
	for _, t := range comp.Tools {
		if toolset[t] {
			kept = append(kept, t)
		}
	}
	comp.Tools = kept
	s.Compositions.Bind(child, comp)
}
