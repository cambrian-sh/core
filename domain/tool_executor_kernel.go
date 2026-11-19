package domain

// Kernel tools served in process, the run ledger record of a call, and the
// artifacts a call writes (split from tool_executor.go, ENG-1531).

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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

// artifactRef mirrors the "_artifacts" entries a handler may inject: the
// jail-relative path, the GC'd-ContentStore CID, and the byte size. (Their one
// producer, the native tool.py ProcessHandler, was deleted with ENG-1421.)
type artifactRef struct {
	Path  string `json:"path"`
	CID   string `json:"cid"`
	Bytes int    `json:"bytes"`
}

// persistArtifacts moves a tool's swept files from the ephemeral (GC-eligible)
// ContentStore into the DURABLE artifact registry — a kind=artifact run-ledger
// entry each, classified at write by the ledger writer and retrievable via
// GetArtifact by its ledger id (ADR-0157 D5.2) — and materializes them to the
// operator output dir so a requested file actually lands on disk. Best-effort and
// nil-safe: a missing dependency or a per-file error degrades with a WARN (the
// tool call already succeeded); it never fails the call. A system call records
// nothing (no principal's run to attribute it to), as for every ledger write.
func (e *ToolExecutor) persistArtifacts(ctx context.Context, req ToolCallRequest, result []byte) {
	record := e.Ledger != nil && !req.System
	if e.ContentStore == nil || (!record && e.ArtifactOutputDir == "") {
		return
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(result, &obj) != nil || obj["_artifacts"] == nil {
		return
	}
	var refs []artifactRef
	if json.Unmarshal(obj["_artifacts"], &refs) != nil {
		return
	}
	for _, r := range refs {
		node, err := e.ContentStore.Get(ctx, CID(r.CID))
		if err != nil || node == nil {
			slog.Warn("tool artifact promote: content fetch failed", "tool", req.ToolName, "cid", r.CID, "err", err)
			continue
		}
		if record {
			pathArg, _ := json.Marshal(map[string]string{"path": r.Path})
			if _, lerr := e.Ledger.Record(ctx, LedgerRecord{
				Kind: LedgerArtifact, Provenance: LedgerProvenanceFromContext(ctx),
				ToolName: req.ToolName, ArgsJSON: pathArg, Writer: AgentPrincipal(req.AgentID),
				InstanceID: AgentInstanceFromContext(ctx), ContentType: contentTypeFor(r.Path), Body: node.Data,
			}); lerr != nil {
				slog.Warn("ADR-0157: tool artifact not recorded to the ledger", "tool", req.ToolName, "path", r.Path, "err", lerr)
			}
		}
		// Materialize to the operator output dir so the user gets the actual file.
		if e.ArtifactOutputDir != "" {
			e.materialize(req.ToolName, r.Path, node.Data)
		}
	}
}

// materialize writes one artifact to the operator output dir. The jail-relative
// path is root-anchored and cleaned so a malicious "../" cannot escape the dir.
func (e *ToolExecutor) materialize(toolName, rel string, data []byte) {
	clean := filepath.Clean("/" + filepath.ToSlash(rel)) // strip any leading ../, anchor at root
	dest := filepath.Join(e.ArtifactOutputDir, filepath.FromSlash(clean))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		slog.Warn("tool artifact materialize: mkdir failed", "tool", toolName, "dest", dest, "err", err)
		return
	}
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		slog.Warn("tool artifact materialize: write failed", "tool", toolName, "dest", dest, "err", err)
		return
	}
	slog.Info("tool artifact materialized", "tool", toolName, "path", rel, "dest", dest)
}

// contentTypeFor infers a coarse MIME type from a file extension for the artifact
// metadata record; unknown extensions default to octet-stream.
func contentTypeFor(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".txt", ".md", ".log", ".csv":
		return "text/plain; charset=utf-8"
	case ".json":
		return "application/json"
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}
