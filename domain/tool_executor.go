package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ArtifactByteWriter persists artifact bytes into the durable content-addressable
// vault, returning the content hash. Satisfied by *vault.ArtifactVault.
type ArtifactByteWriter interface {
	Store(content []byte) (string, error)
}

// ArtifactRecorder persists an artifact's durable metadata record (retrievable
// via GetArtifact, scope-governed). Satisfied by the agent repository decorator.
type ArtifactRecorder interface {
	SaveArtifact(a Artifact) error
}

// ToolOutputRecorder feeds a successful tool output into the memory layer. A READ
// output (ADR-0048 D6) flows into the Tier-1 → Tier-2 curation pipeline as a
// `mnemonic_fact` (its payload is knowledge). A MUTATION output (ADR-0049 D1) is
// recorded as a `mnemonic_action` EVENT instead — what was done, not knowledge.
// Routing is the caller's deterministic read/write classification. Satisfied by the
// MemoryAgent.
type ToolOutputRecorder interface {
	RecordToolOutput(ctx context.Context, rec ToolOutputRecord) error
}

// ToolOutputRecord carries one tool call's output to the recorder. IsMutation (the
// tool has DataWriteKinds — ADR-0034) routes it to an action record vs. a fact;
// ArgsJSON is needed to format the action line ("what was done where").
type ToolOutputRecord struct {
	ToolName   string
	ArgsJSON   []byte
	Output     []byte
	IsMutation bool
	TaskID     string // ADR-0049 D3: per-step correlation key stamped on action records
}

// FormatActionLine renders a mutation tool call into a compact, deterministic action
// line — `<tool> → <status> | k=v, …` (ADR-0049 D1). No raw payload, no LLM; large
// arg values collapse to a `<N chars>` marker so "wrote 8KB" doesn't inline 8KB.
func FormatActionLine(toolName string, argsJSON, output []byte) string {
	args := condenseActionArgs(argsJSON)
	if args == "" {
		return fmt.Sprintf("%s → %s", toolName, actionStatus(output))
	}
	return fmt.Sprintf("%s → %s | %s", toolName, actionStatus(output), args)
}

func actionStatus(output []byte) string {
	var obj map[string]json.RawMessage
	if json.Unmarshal(output, &obj) == nil {
		if _, ok := obj["denied"]; ok {
			return "denied"
		}
		if _, ok := obj["error"]; ok {
			return "error"
		}
		if isFailedShellResult(obj) {
			return "error"
		}
	}
	return "ok"
}

const actionArgValueCap = 80

// condenseActionArgs renders args as sorted `k=v` pairs, collapsing any large value
// to `<N chars>` (deterministic; the path/url stays, the content payload does not).
func condenseActionArgs(argsJSON []byte) string {
	var args map[string]json.RawMessage
	if len(argsJSON) == 0 || json.Unmarshal(argsJSON, &args) != nil {
		return ""
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		var s string
		if json.Unmarshal(args[k], &s) != nil {
			s = string(args[k]) // non-string value: its compact JSON
		}
		if len(s) > actionArgValueCap {
			s = fmt.Sprintf("<%d chars>", len(s))
		}
		parts = append(parts, fmt.Sprintf("%s=%s", k, s))
	}
	return strings.Join(parts, ", ")
}

// isDeniedResult reports whether a tool result is a denial (the call did not run).
func isDeniedResult(result []byte) bool {
	var obj map[string]json.RawMessage
	if json.Unmarshal(result, &obj) == nil {
		_, ok := obj["denied"]
		return ok
	}
	return false
}

// shouldPromoteToolOutput is the deterministic COST pre-filter in front of Tier-2
// curation (ADR-0048 D6): skip below a size floor and skip results that are
// themselves a failure. It is cost control, not value-routing — the keep/drop
// *value* judgment stays in the Tier-2 LLM (Zero-Hardcode).
//
// Failures are skipped here, at the RAW result, on purpose: downstream the
// MemoryAgent wraps the output in a "tool[name]: …" envelope, which is no longer
// valid JSON and so defeats Tier-2's JSON error detection (checkJSONErrorPayload).
// A shell failure (non-zero exit_code, or stderr with no stdout — the BusyBox
// "ps: unknown option" shape) therefore has to be caught here or it leaks into LTM.
func shouldPromoteToolOutput(result []byte, minBytes int) bool {
	if len(result) < minBytes {
		return false
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(result, &obj) == nil {
		if _, isErr := obj["error"]; isErr {
			return false
		}
		if _, isDenied := obj["denied"]; isDenied {
			return false
		}
		if isFailedShellResult(obj) {
			return false
		}
	}
	return true
}

// isFailedShellResult reports whether a parsed tool result is a failed shell
// invocation: a non-zero exit_code, or a non-empty stderr paired with empty
// stdout. Mirrors checkJSONErrorPayload's semantics so the two pre-filters agree
// on what "a shell failure" is. A result carrying real stdout is NOT treated as a
// failure even if it also emits stderr (warnings) — that output may be worth keeping.
func isFailedShellResult(obj map[string]json.RawMessage) bool {
	if raw, ok := obj["exit_code"]; ok {
		var code int
		if json.Unmarshal(raw, &code) == nil && code != 0 {
			return true
		}
	}
	stderr := jsonString(obj["stderr"])
	stdout := jsonString(obj["stdout"])
	return stderr != "" && stdout == ""
}

// jsonString decodes a RawMessage as a string, returning "" when absent or not a string.
func jsonString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// ToolCallRequest is the kernel-internal request for one tool invocation
// (the ExecuteTool RPC maps onto this). The principal is resolved from gRPC
// metadata, never from the args.
type ToolCallRequest struct {
	AgentID  string
	ToolName string
	ArgsJSON []byte
	// SessionTokenID is the agent's per-step managed-LLM session token (ADR-0018).
	// Carried so the executor can recognize a sandboxed evaluation session and
	// auto-approve dangerous tools within it (see EvaluationSessionSet). Empty for
	// callers that do not run under a managed session.
	SessionTokenID string
	// TaskID is the per-step correlation key (ADR-0049 D3, step-{index}-{planID}),
	// read from x-task-id metadata. Stamps action records so a step's actions can be
	// counted at step-end for the prose-synthesis dedup. "" leaves dedup off.
	TaskID string
	// System marks an operator ScopeSystem execution (ADR-0047 Amendment A2.2). It
	// bypasses the per-agent grant (allow-all policy), the data-store scope, and the
	// dangerous-tool approval gate — the operator is above the scope plane (D13).
	// The resource-arg policy and process confinement STILL apply. Never set from an
	// agent-facing path: the operator command handler is the only producer, and it
	// audits the call + emits a dangerous-tool feed event.
	System bool
}

// ToolCallResponse is the structured outcome of an ExecuteTool call. It never
// represents a crash: a denial, a handler error, and a success are all values.
type ToolCallResponse struct {
	ResultJSON []byte
	ResultCID  string // set when the result was offloaded to the ContentStore
	Denied     bool
	DenyReason string
	Error      string
	ArgHash    string
	ResultHash string
	ApproverID string
}

// ToolExecutor is the single reference monitor for tool execution (ADR-0039 D4).
// It authorizes (grant + resource policy + data scope + approval) entirely
// kernel-side and pre-invocation (A1.4), then dispatches to the handler, audits,
// and offloads large results. It never panics.
type ToolExecutor struct {
	Registry        ToolRegistry
	Grants          GrantsProvider
	Handler         ToolHandler           // the confined Python tool-process invoker (A1.2)
	MCPHandler      ToolHandler           // ADR-0043: invokes external MCP tools (mcp:<server>/<tool>); nil ⇒ none
	Approval        ApprovalController    // nil ⇒ dangerous tools are denied (fail-closed)
	EvalSessions    EvaluationSessionSet  // nil ⇒ no session is an evaluation (operator approval applies)
	EgressAuditor   EgressAuditor         // ADR-0043: records remote-tool data egress; nil ⇒ no auditing
	Retriever       ToolRetriever         // ADR-0044: relevance-ranks the granted menu; nil ⇒ full menu
	Scope           ToolScopeResolver     // nil ⇒ data-regime tools are denied (fail-closed)
	ContentStore    ContentStore          // nil ⇒ results returned inline
	InlineThreshold int                   // results larger than this go to the ContentStore
	// ADR-0043 budget regime: when both are set, a priced tool call is reserved
	// against the session budget before dispatch and reconciled to actual after.
	// nil ⇒ tool calls are unmetered (no behaviour change).
	Budget  *BudgetLedger
	Pricing ToolPricingSource
	// Unrestricted is the operator-chosen bypass (ExecutionConfig.ToolsUnrestricted):
	// every named agent may call every registered tool with an allow-all resource
	// policy. Approval for dangerous tools STILL applies; an anonymous principal is
	// still denied. Trusted/dev deployments only.
	Unrestricted bool
	// RestrictedTools (ADR-0051 D6) caps which tools a principal may use, as a HARD
	// CEILING that overrides Unrestricted: principal id → the set of tool names it may
	// touch. A principal absent from the map is unrestricted (normal resolution). A
	// principal present may use ONLY its listed tools, even under the Unrestricted bypass
	// — fail-closed for everything else. This is how the Scout principal is confined to
	// the operator's `discovery-safe` set (never a write/dangerous tool), since it fires
	// constantly and unattended at plan time. Generic: the executor knows nothing of Scout.
	RestrictedTools map[string]map[string]bool
	// Overlay holds run-scoped grants conferred by a loaded system skill (ADR-0046
	// D6). nil ⇒ no skill-conferred grants. Consulted in grantFor after the static
	// grants; dangerous tools still require approval regardless of how granted.
	Overlay *RunGrantOverlay
	// Artifact promotion (the durable home for files a confined tool writes). The
	// handler offloads jail files to the GC'd ContentStore and surfaces them in the
	// result "_artifacts"; without promotion they are unretrievable and eventually
	// evicted. When wired, each is also stored in the durable vault + metadata
	// (retrievable via GetArtifact, scope-governed) and materialized to disk.
	ArtifactBytes     ArtifactByteWriter                       // nil ⇒ no durable vault promotion
	ArtifactMeta      ArtifactRecorder                         // nil ⇒ no durable metadata record
	ArtifactTags      func(ctx context.Context, agentID string) []string // kernel-derived write tags; nil ⇒ none
	ArtifactOutputDir string                                   // "" ⇒ no on-disk materialization
	// ADR-0048 D6: tool-output promotion to LTM. When wired, a successful tool
	// output that clears the cost pre-filter is fed to Tier-1/Tier-2 curation, where
	// the LLM scorer decides keep/drop. nil ⇒ no promotion.
	ToolOutput         ToolOutputRecorder
	ToolOutputMinBytes int // size floor for the pre-filter (0 ⇒ promote any non-error output)
}

func denied(reason string, argHash string) ToolCallResponse {
	return ToolCallResponse{Denied: true, DenyReason: reason, ArgHash: argHash}
}

// priceFor returns the pricing for a call when the budget regime is wired and the
// tool is priced under a managed session (ADR-0043). Without a session token the
// call is unattributable, so it is left unmetered.
func (e *ToolExecutor) priceFor(req ToolCallRequest) (ToolPricing, bool) {
	if e.Budget == nil || e.Pricing == nil || req.SessionTokenID == "" {
		return ToolPricing{}, false
	}
	return e.Pricing.PricingFor(req.ToolName)
}

// budgetAccountFor keys a call's spend to its managed session (ADR-0043 D5).
func budgetAccountFor(req ToolCallRequest) string {
	return "mcp:" + req.SessionTokenID
}

// Execute authorizes and runs one tool call.
// hydrateCIDArgs resolves pass-by-reference tool arguments (ADR-0048 #3). A value
// shaped {"$cid":"<cid>"} is replaced with the full content stored at that CID, so an
// agent can reference EXISTING content (a recalled fact, a prior step's offloaded
// output) without re-emitting it inline — which is token-cheap AND sidesteps the
// truncation hazard of large inline payloads. Reads are gated by CanReadContentNode:
// an agent may hydrate ownerless/system content or its own session's blobs, never
// another session's private content. Best-effort — an unresolvable or unauthorized
// ref is left untouched (the tool then receives the literal ref, never another
// session's data).
func (e *ToolExecutor) hydrateCIDArgs(ctx context.Context, argsJSON []byte) []byte {
	if e.ContentStore == nil || len(argsJSON) == 0 {
		return argsJSON
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(argsJSON, &args) != nil {
		return argsJSON
	}
	sid, _ := SessionIDFromContext(ctx)
	changed := false
	for k, v := range args {
		var ref struct {
			CID string `json:"$cid"`
		}
		if json.Unmarshal(v, &ref) != nil || ref.CID == "" {
			continue
		}
		node, err := e.ContentStore.Get(ctx, CID(ref.CID))
		if err != nil || node == nil || !CanReadContentNode(node.OwnerSession, sid) {
			continue
		}
		if b, err := json.Marshal(string(node.Data)); err == nil {
			args[k] = b
			changed = true
		}
	}
	if !changed {
		return argsJSON
	}
	if out, err := json.Marshal(args); err == nil {
		return out
	}
	return argsJSON
}

func (e *ToolExecutor) Execute(ctx context.Context, req ToolCallRequest) ToolCallResponse {
	argHash := hashBytes(req.ArgsJSON)

	tool, ok := e.Registry.Get(req.ToolName)
	if !ok {
		return denied("unknown tool", argHash)
	}

	// Grant (fail-closed on unknown principal / no grant). A2.2: an operator
	// ScopeSystem execution (req.System) bypasses the per-agent grant with an
	// allow-all policy — the operator is above the scope plane (D13).
	var grant ToolGrant
	if req.System {
		grant = ToolGrant{Tool: req.ToolName, Policy: ToolResourcePolicy{AllowAll: true}}
	} else {
		g, granted := e.grantFor(ctx, req.AgentID, req.ToolName, req.SessionTokenID)
		if !granted {
			return denied("tool not granted to agent", argHash)
		}
		grant = g
	}

	// ADR-0048 #3: resolve {"$cid":"…"} reference args to their stored content before
	// policy + dispatch, so the tool receives the real bytes. Done after the grant
	// check (no CAS reads for ungranted calls); the argHash above stays keyed on the
	// logical reference action.
	req.ArgsJSON = e.hydrateCIDArgs(ctx, req.ArgsJSON)

	// Regime 2 — system-resource policy on the tool's declared resource args.
	if reason, ok := checkResourcePolicy(tool, grant.Policy, req.ArgsJSON); !ok {
		return denied("resource policy: "+reason, argHash)
	}

	// Regime 1 — data-store scope, when the tool touches tagged stores. A2.2: an
	// operator ScopeSystem execution reads/writes at ScopeSystem (D13), so it is
	// not scope-gated here.
	if !req.System && (len(tool.DataReadKinds) > 0 || len(tool.DataWriteKinds) > 0) {
		if !e.scopeAllows(ctx, req.AgentID, tool) {
			return denied("scope", argHash)
		}
	}

	// Approval for dangerous tools (fail-closed). A sandboxed evaluation session
	// (the graded interview, ADR-0037) auto-approves: it runs unattended with no
	// operator, and the per-call process sandbox — not a human — is the
	// containment boundary for a synthetic scenario. Without this, every
	// dangerous-tool capability scores as "failed" and corrupts the capability
	// profile that EFE/Gatekeeper priors are built on.
	approver := ""
	if tool.Dangerous && req.System {
		// A2.2: an operator ScopeSystem execution carries its own authority — no
		// per-agent HITL gate. The operator command handler audits the call and
		// emits a dangerous-tool feed event so the privileged action stays visible.
		approver = "operator:system"
	} else if tool.Dangerous {
		if e.EvalSessions != nil && e.EvalSessions.IsEvaluation(req.SessionTokenID) {
			approver = "evaluation-sandbox"
		} else {
			if e.Approval == nil {
				return denied("approval required but unavailable", argHash)
			}
			dec, err := e.Approval.Request(ctx, ApprovalRequest{
				AgentID: req.AgentID, ToolName: req.ToolName, ArgsPreview: preview(req.ArgsJSON, 200),
			})
			if err != nil || !dec.Approved {
				return ToolCallResponse{Denied: true, DenyReason: "not approved", ArgHash: argHash, ApproverID: dec.ApproverID}
			}
			approver = dec.ApproverID
		}
	}

	// Dispatch. ADR-0043: an mcp:<server>/<tool> identity routes to the MCP
	// handler; everything else runs as a confined native subprocess. Both
	// implement ToolHandler, so authorization above is identical.
	handler := e.Handler
	isMCP := strings.HasPrefix(req.ToolName, "mcp:")
	if isMCP && e.MCPHandler != nil {
		handler = e.MCPHandler
	}
	// ADR-0043 D4: a remote MCP call sends the agent's args outside the trust
	// boundary — record the egress (the call itself is allowed; the operator owns
	// endpoint trust, and Regime-1 above already enforced any declared data class).
	if isMCP && e.EgressAuditor != nil {
		e.EgressAuditor.RecordEgress(req.AgentID, req.ToolName, tool.DataWriteKinds)
	}
	if handler == nil {
		return ToolCallResponse{Error: "no tool handler configured", ArgHash: argHash}
	}

	// ADR-0043 budget regime (admission): reserve the estimated cost before
	// dispatch; deny budget_exhausted if it does not fit the session budget. The
	// hold is reconciled to actual (or released) after the call.
	pricing, priced := e.priceFor(req)
	var holdID string
	if priced {
		id, rerr := e.Budget.Reserve(pricing.Reserve(), budgetAccountFor(req))
		if rerr != nil {
			var be *BudgetExhaustedError
			if errors.As(rerr, &be) {
				return denied("budget_exhausted: "+be.Error(), argHash)
			}
			return ToolCallResponse{Error: rerr.Error(), ArgHash: argHash}
		}
		holdID = id
	}

	result, err := handler.Execute(ctx, ToolCall{ToolName: req.ToolName, ArgsJSON: req.ArgsJSON, Policy: grant.Policy})
	if err != nil {
		if priced {
			// Failure-cost (D7): never-reached ⇒ 0; otherwise the per-server policy.
			reached := !strings.Contains(err.Error(), "not connected")
			_ = e.Budget.Reconcile(holdID, pricing.FailureCost(reached, 0, false))
		}
		return ToolCallResponse{Error: err.Error(), ArgHash: argHash, ResultHash: hashString(err.Error()), ApproverID: approver}
	}
	if priced {
		// Reconcile to actual. Usage-field extraction is a follow-up; absent it,
		// reconcile is exact for flat and cap-on-unmeasurable for per_unit (D6).
		_ = e.Budget.Reconcile(holdID, pricing.Reconcile(0, false))
	}

	// Promote any files the tool wrote (the "_artifacts" the confined handler swept
	// into the GC'd ContentStore) into the durable artifact system + disk, BEFORE
	// the result is (maybe) offloaded and ResultJSON nil'd below.
	e.persistArtifacts(ctx, req, result)

	// Feed the output into memory, routed by the tool's declared nature (ADR-0049 D1).
	// A MUTATION is an EVENT: record it as an action regardless of size (a write that
	// happened is durable history), skipping only a denied call (it never ran). A READ
	// is knowledge: keep the ADR-0048 D6 cost floor + error pre-filter before Tier-2.
	// Best-effort — never fails the call.
	if e.ToolOutput != nil {
		isMutation := len(tool.DataWriteKinds) > 0
		if isMutation {
			if !isDeniedResult(result) {
				_ = e.ToolOutput.RecordToolOutput(ctx, ToolOutputRecord{
					ToolName: req.ToolName, ArgsJSON: req.ArgsJSON, Output: result, IsMutation: true, TaskID: req.TaskID,
				})
			}
		} else if shouldPromoteToolOutput(result, e.ToolOutputMinBytes) {
			_ = e.ToolOutput.RecordToolOutput(ctx, ToolOutputRecord{
				ToolName: req.ToolName, ArgsJSON: req.ArgsJSON, Output: result, IsMutation: false, TaskID: req.TaskID,
			})
		}
	}

	resp := ToolCallResponse{ResultJSON: result, ArgHash: argHash, ResultHash: hashBytes(result), ApproverID: approver}
	// Offload large results to CAS.
	if e.ContentStore != nil && e.InlineThreshold > 0 && len(result) > e.InlineThreshold {
		if cid, serr := e.ContentStore.Put(ctx, result, "tool_result", nil, preview(result, 500)); serr == nil {
			resp.ResultCID = string(cid)
			resp.ResultJSON = nil
		}
	}
	return resp
}

// artifactRef mirrors the "_artifacts" entries the ProcessHandler injects: the
// jail-relative path, the GC'd-ContentStore CID, and the byte size.
type artifactRef struct {
	Path  string `json:"path"`
	CID   string `json:"cid"`
	Bytes int    `json:"bytes"`
}

// persistArtifacts moves a tool's swept files from the ephemeral (GC-eligible)
// ContentStore into the DURABLE artifact system — the vault + a metadata record,
// retrievable via GetArtifact and scope-governed — and materializes them to the
// operator output dir so a requested file actually lands on disk. Best-effort and
// nil-safe: a missing dependency or a per-file error degrades silently (the tool
// call already succeeded); it never fails the call.
func (e *ToolExecutor) persistArtifacts(ctx context.Context, req ToolCallRequest, result []byte) {
	if e.ContentStore == nil || (e.ArtifactBytes == nil && e.ArtifactOutputDir == "") {
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
	var tags []string
	if e.ArtifactTags != nil {
		tags = e.ArtifactTags(ctx, req.AgentID)
	}
	for _, r := range refs {
		node, err := e.ContentStore.Get(ctx, CID(r.CID))
		if err != nil || node == nil {
			slog.Warn("tool artifact promote: content fetch failed", "tool", req.ToolName, "cid", r.CID, "err", err)
			continue
		}
		// Durable vault + metadata record (survives content-store GC; retrievable
		// via GetArtifact under the agent's kernel-derived write classification).
		if e.ArtifactBytes != nil && e.ArtifactMeta != nil {
			if hash, serr := e.ArtifactBytes.Store(node.Data); serr == nil {
				if rerr := e.ArtifactMeta.SaveArtifact(Artifact{
					Hash:        hash,
					ContentType: contentTypeFor(r.Path),
					SizeBytes:   int64(len(node.Data)),
					SessionID:   req.SessionTokenID,
					Tags:        tags,
				}); rerr != nil {
					slog.Warn("tool artifact promote: metadata record failed", "tool", req.ToolName, "path", r.Path, "err", rerr)
				}
			} else {
				slog.Warn("tool artifact promote: vault store failed", "tool", req.ToolName, "path", r.Path, "err", serr)
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

// AvailableTools returns the system tools an agent may invoke, for building the
// agent's ReAct prompt menu (ADR-0039 / SDK ReAct routing). It mirrors grantFor's
// authority rules so the menu matches what Execute will actually allow: an
// anonymous principal sees nothing; under the unrestricted bypass every named
// agent sees every registered tool; otherwise the agent sees exactly the tools
// named by its grants. This is an advisory menu only — Execute still authorizes
// (grant + resource policy + scope + approval) every call (A1.4).
// AllTools returns the whole registered tool catalog, independent of any agent's
// grants. The operator plane governs the catalog at ScopeSystem (ADR-0047
// Amendment A2.3) — distinct from AvailableTools, which is a per-agent
// grant-filtered advisory menu. nil registry ⇒ empty catalog.
func (e *ToolExecutor) AllTools() []SystemTool {
	if e.Registry == nil {
		return nil
	}
	return e.Registry.All()
}

func (e *ToolExecutor) AvailableTools(ctx context.Context, agentID string) []SystemTool {
	if agentID == "" {
		return nil // anonymous principal: no menu, same as fail-closed grantFor
	}
	var menu []SystemTool
	if e.Unrestricted {
		menu = e.Registry.All()
	} else {
		grants, err := e.Grants.GrantsFor(ctx, agentID)
		if err != nil {
			return nil
		}
		menu = e.Registry.SchemasFor(grants)
	}
	// ADR-0051 D6: a restricted principal (the Scout) sees only its allowlisted tools, so
	// the advisory menu matches what grantFor will actually allow.
	if allow, restricted := e.RestrictedTools[agentID]; restricted {
		filtered := menu[:0:0]
		for _, t := range menu {
			if allow[t.Name] {
				filtered = append(filtered, t)
			}
		}
		return filtered
	}
	return menu
}

// AvailableToolsRanked returns the agent's granted tools narrowed to the top-k
// most relevant to query (ADR-0044). Authorization is unchanged — it grant-filters
// via AvailableTools first, then ranks within that authorized set, so an ungranted
// tool can never appear. Degrades to the full menu when there is no query, no
// retriever, the set already fits in k, or ranking errors. An empty ranked result
// (the retriever's relevance floor cleared nothing) is honored — the menu is empty.
func (e *ToolExecutor) AvailableToolsRanked(ctx context.Context, agentID, query string, k int) []SystemTool {
	full := e.AvailableTools(ctx, agentID)
	if query == "" || e.Retriever == nil || k <= 0 || len(full) <= k {
		return full
	}
	names := make([]string, len(full))
	byName := make(map[string]SystemTool, len(full))
	for i, t := range full {
		names[i] = t.Name
		byName[t.Name] = t
	}
	ranked, err := e.Retriever.Rank(ctx, query, names, k)
	if err != nil {
		return full // degrade to the full menu on a ranking failure, never crash
	}
	out := make([]SystemTool, 0, len(ranked))
	for _, n := range ranked {
		if t, ok := byName[n]; ok {
			out = append(out, t)
		}
	}
	return out
}

// AvailableToolsNamed returns the named tools the agent may invoke — the
// describe_tool Tier-2 fetch (ADR-0045 D4/D6). It grant-filters via
// AvailableTools first, then keeps only the requested names, so an
// ungranted/unknown name is simply absent (fail-closed, no existence leak). An
// anonymous principal gets nothing.
func (e *ToolExecutor) AvailableToolsNamed(ctx context.Context, agentID string, names []string) []SystemTool {
	if len(names) == 0 {
		return nil
	}
	want := make(map[string]struct{}, len(names))
	for _, n := range names {
		want[n] = struct{}{}
	}
	var out []SystemTool // nil when nothing matches (anonymous / all ungranted)
	for _, t := range e.AvailableTools(ctx, agentID) {
		if _, ok := want[t.Name]; ok {
			out = append(out, t)
		}
	}
	return out
}

func (e *ToolExecutor) grantFor(ctx context.Context, agentID, tool, sessionToken string) (ToolGrant, bool) {
	if agentID == "" {
		return ToolGrant{}, false // anonymous principal denied even when unrestricted
	}
	// ADR-0051 D6: a restricted principal (e.g. the Scout) may use ONLY its allowlisted
	// (discovery-safe) tools — a hard ceiling enforced BEFORE the Unrestricted bypass, so
	// it holds even in dev/unrestricted mode. Everything else is denied fail-closed.
	if allow, restricted := e.RestrictedTools[agentID]; restricted && !allow[tool] {
		return ToolGrant{}, false
	}
	// Unrestricted: any named agent gets an allow-all grant for any registered
	// tool (operator-chosen bypass). Approval for dangerous tools still applies.
	if e.Unrestricted {
		return ToolGrant{Tool: tool, Policy: ToolResourcePolicy{AllowAll: true}}, true
	}
	grants, err := e.Grants.GrantsFor(ctx, agentID)
	if err != nil {
		return ToolGrant{}, false
	}
	for _, g := range grants {
		if g.Tool == tool {
			return g, true
		}
	}
	// ADR-0046 D6: a system skill loaded this run may have conferred this tool
	// (run-scoped overlay, keyed by the session token). The conferred grant carries
	// an allow-all resource policy — the operator vouched for the skill — but
	// dangerous tools still hit ApprovalController in Execute.
	if e.Overlay.Granted(sessionToken, tool) {
		return ToolGrant{Tool: tool, Policy: ToolResourcePolicy{AllowAll: true}}, true
	}
	return ToolGrant{}, false
}

// ConferSkillGrants activates a loaded system skill's tool grants run-scoped
// (ADR-0046 D6). A system skill is operator-authored, so it MAY confer tools the
// agent otherwise lacks, for the duration of the run (keyed by session). No-op
// without an overlay/session. Agent-local skills never call this — their grants
// are already within the agent's envelope (narrow-only).
func (e *ToolExecutor) ConferSkillGrants(session string, tools []string) {
	e.Overlay.Activate(session, tools)
}

func (e *ToolExecutor) scopeAllows(ctx context.Context, agentID string, tool SystemTool) bool {
	if e.Scope == nil {
		return false // fail-closed: a data tool with no scope resolver is denied
	}
	eff, ok := e.Scope.EffectiveForAgent(ctx, agentID)
	if !ok {
		return false
	}
	if !eff.Allows(tool.DataReadKinds) {
		return false
	}
	if len(tool.DataWriteKinds) > 0 && !eff.Allows(tool.DataWriteKinds) {
		return false
	}
	return true
}

// checkResourcePolicy validates each declared resource arg against the policy.
// Returns (reason, false) on the first violation.
func checkResourcePolicy(tool SystemTool, pol ToolResourcePolicy, argsJSON []byte) (string, bool) {
	if len(tool.PathArgs) == 0 && len(tool.URLArgs) == 0 && len(tool.CommandArgs) == 0 {
		return "", true
	}
	var args map[string]any
	if len(argsJSON) > 0 {
		if err := json.Unmarshal(argsJSON, &args); err != nil {
			return "unparseable args", false
		}
	}
	for _, a := range tool.PathArgs {
		if v, ok := strArg(args, a); ok && !pol.AllowsPath(v) {
			return fmt.Sprintf("path %q not permitted", a), false
		}
	}
	for _, a := range tool.URLArgs {
		if v, ok := strArg(args, a); ok && !pol.AllowsURL(v) {
			return fmt.Sprintf("url %q not permitted", a), false
		}
	}
	for _, a := range tool.CommandArgs {
		if v, ok := strArg(args, a); ok && !pol.AllowsCommand(v) {
			return fmt.Sprintf("command %q not permitted", a), false
		}
	}
	return "", true
}

func strArg(args map[string]any, key string) (string, bool) {
	v, ok := args[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func hashBytes(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hashString(s string) string { return hashBytes([]byte(s)) }

func preview(b []byte, n int) string {
	s := string(b)
	if len(s) <= n {
		return s
	}
	return s[:n]
}
