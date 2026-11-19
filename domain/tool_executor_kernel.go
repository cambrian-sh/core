package domain

// Kernel tools served in process, the run ledger record of a call, and the
// artifacts a call writes (split from tool_executor.go, ENG-1531).

import (
	"context"
	"encoding/json"
	"log/slog"
)

// kernelTool returns the kernel tool named name, or nil.
func (e *ToolExecutor) kernelTool(name string) KernelTool {
	for _, kt := range e.KernelTools {
		if kt.Spec().Name == name {
			return kt
		}
	}
	return nil
}

// KernelToolSpecs returns the menu entries of the kernel tools (ListTools appends
// them to every agent's menu).
func (e *ToolExecutor) KernelToolSpecs() []SystemTool {
	if e == nil {
		return nil
	}
	out := make([]SystemTool, 0, len(e.KernelTools))
	for _, kt := range e.KernelTools {
		out = append(out, kt.Spec())
	}
	return out
}

// KernelToolSpecsFor is KernelToolSpecs for the holder of lease: a kernel tool
// that implements KernelToolOffer is listed only when it is offered to that lease
// (run_subtasks: the single-agent lane's agent below max depth, ADR-0150 D2/D6).
// Hiding is only the menu; the tool refuses an unoffered caller itself.
func (e *ToolExecutor) KernelToolSpecsFor(lease LeaseID) []SystemTool {
	if e == nil {
		return nil
	}
	out := make([]SystemTool, 0, len(e.KernelTools))
	for _, kt := range e.KernelTools {
		if o, ok := kt.(KernelToolOffer); ok && !o.OfferedTo(lease) {
			continue
		}
		out = append(out, kt.Spec())
	}
	return out
}

// runKernelTool serves a kernel tool for a named caller, under the caller's own
// read predicate (resolved from the decision point exactly as GetArtifact resolves
// it, and carried on ctx for the enforced reader, AGENTS.md §2.4).
func (e *ToolExecutor) runKernelTool(ctx context.Context, req ToolCallRequest, kt KernelTool, argHash string) ToolCallResponse {
	eff, reason := e.callerReadScope(ctx, req)
	if eff == nil {
		return denied(reason, argHash)
	}
	result, err := kt.Call(WithScope(ctx, eff), KernelToolCall{AgentID: req.AgentID, ArgsJSON: req.ArgsJSON, SessionTokenID: req.SessionTokenID})
	// ENG-531: what a kernel tool hands back is content the session consumed,
	// under the tool's own provenance (KernelToolSource; SourceTool otherwise).
	class := SourceTool
	if ks, ok := kt.(KernelToolSource); ok {
		class = ks.ResultSource()
	}
	if !req.System {
		e.observe(ctx, class, kt.Spec().AuthzTags())
	}
	if err != nil {
		return ToolCallResponse{Error: err.Error(), ArgHash: argHash, ResultHash: hashString(err.Error()), SourceClass: class}
	}
	return ToolCallResponse{ResultJSON: result, ArgHash: argHash, ResultHash: hashBytes(result), SourceClass: class}
}

// callerReadScope resolves the caller's own read predicate from the decision
// point, exactly as GetArtifact resolves it (AGENTS.md §2.4). nil with the reason
// when the caller may read nothing (an anonymous principal, a refused filter).
func (e *ToolExecutor) callerReadScope(ctx context.Context, req ToolCallRequest) (*TagPredicate, string) {
	if req.AgentID == "" && !req.System {
		return nil, "anonymous principal"
	}
	a := e.authz
	if a == nil {
		a = AllowAllAuthorizer{}
	}
	principal := AgentPrincipal(req.AgentID)
	if req.System {
		principal = SystemPrincipal
	}
	// The turn's surface (ADR-0085 D7): a surface-linked policy binds kernel-tool
	// reads and staged inputs exactly as it binds the effect decision.
	eff, dec := a.ReadFilter(ctx, principal, SurfaceFromContextOr(ctx, SurfaceRef{Kind: SurfaceAgent}))
	if eff == nil {
		return nil, dec.Explain()
	}
	return eff, ""
}

// recordToLedger writes one call's result to the run ledger (ADR-0145 D1).
// Best-effort for the CALL: a failed write never fails the tool call, it is
// logged and the caller gets the result inline. Returns the entry id, "" when
// nothing was recorded.
// derivedFrom names the ledger entries the call's result was computed from (a
// staged call's inputs), so the entry is never easier to read than they are.
func (e *ToolExecutor) recordToLedger(ctx context.Context, req ToolCallRequest, tool SystemTool, result []byte, derivedFrom []string) string {
	if e.Ledger == nil || req.System {
		// An operator ScopeSystem execution is not part of any turn; it is audited
		// by the operator command handler instead.
		return ""
	}
	ct := "text/plain"
	if json.Valid(result) {
		ct = "application/json"
	}
	ent, err := e.Ledger.Record(ctx, LedgerRecord{
		Kind:               LedgerToolResult,
		Provenance:         LedgerProvenanceFromContext(ctx),
		ToolName:           req.ToolName,
		ArgsJSON:           req.ArgsJSON,
		Writer:             AgentPrincipal(req.AgentID),
		ClassificationHint: tool.AuthzTags(),
		DerivedFrom:        derivedFrom,
		ContentType:        ct,
		Body:               result,
	})
	if err != nil {
		slog.Warn("ADR-0145: tool result not recorded to the ledger", "tool", req.ToolName, "agent", req.AgentID, "err", err)
		return ""
	}
	return ent.ID
}

// previewChars is PreviewChars with its default (execution.content.preview_chars).
func (e *ToolExecutor) previewChars() int {
	if e.PreviewChars > 0 {
		return e.PreviewChars
	}
	return 600
}
