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
	"strconv"
	"strings"
	"time"
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
	// FactEligible reports whether this output passed the ADR-0048 D6 cost floor and
	// is therefore worth curating as a FACT. It does not gate the world model.
	//
	// The two were previously the same decision, and that was a hole. The size floor
	// asks "is this output substantial enough to embed as knowledge"; the world model
	// asks "what did this call touch". A tiny result — an MCP write confirming success
	// in 30 bytes — fails the first and is highly relevant to the second. Conflating
	// them meant every small tool call vanished from the world model, so plan scenes
	// came back contentless and no outcome record was ever written, with nothing
	// logged to say why.
	FactEligible bool
	// ClassificationTags are the tool's own domain tags (AuthzTags), carried so the
	// LTM write is classified from principal + tool domain. The pre-invocation gates
	// already consume these; the artifact path already classifies its writes — this
	// closes the one destination that landed untagged.
	ClassificationTags []string
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
	// SessionTokenID is the agent's per-step managed-LLM BudgetLease (ADR-0018).
	// Carried so the executor can recognize a sandboxed evaluation session and
	// auto-approve dangerous tools within it (see EvaluationSessionSet). Empty for
	// callers that do not run under a managed session.
	SessionTokenID LeaseID
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
	Registry   ToolRegistry
	Grants     GrantsProvider
	Handler    ToolHandler // the confined Python tool-process invoker (A1.2)
	MCPHandler ToolHandler // ADR-0043: invokes external MCP tools (mcp:<server>/<tool>); nil ⇒ none
	// ADR-0127 (contribution lane, CL-0): the owner-scoped source of contributed
	// worker tools, and the handler that relays an authorized
	// local:<machine>/<tool> step to its worker. Fleet nil ⇒ no lane: every
	// local: call refuses fail-closed. LocalHandler nil ⇒ an in-scope local:
	// step reports "no tool handler configured" (CL-0 wires the honest
	// LocalRelayStub; the real relay is CL-1). Contributed tools NEVER live in
	// Registry — they resolve per call from the task beneficiary's fleet.
	Fleet        FleetSource
	LocalHandler ToolHandler
	// FleetDecisions, when non-nil, receives the contributed-lane decisions:
	// the dispatch-layer scoping refusals (most importantly cross-owner, the
	// lane's load-bearing invariant made observable, ADR-0127 D9) and — CL-2 —
	// every consent outcome and parking event, allow and deny alike. Everything
	// is logged regardless; this seam is for tests and for the premium decision
	// journal.
	FleetDecisions func(AccessDecision)
	// Consent is the CL-2 pre-dispatch consent gate (ADR-0127 D7) and the
	// selection ladder's rung-4 "which machine?" channel (D6): prompts route to
	// the initiating conversation surface through it, mirroring the
	// ApprovalController shape. nil ⇒ FAIL-CLOSED: any contributed step that
	// needs a prompt (effectful under auto, anything under any-surface, a
	// ladder question) refuses with a recorded decision; read-only steps under
	// auto and on-machine-only steps still dispatch (their consent path does
	// not run through the kernel prompt).
	Consent ConsentController
	// ParkDeadline bounds how long a contributed step whose target machine is
	// offline stays parked awaiting the machine's return (ADR-0127 D6;
	// mcp.worker_park_deadline_ms). 0 ⇒ 24h. Parked steps are in-memory with
	// the task's own lifetime — a kernel restart drops them (the phase-4
	// residual).
	ParkDeadline  time.Duration
	Approval      ApprovalController   // nil ⇒ dangerous tools are denied (fail-closed)
	EvalSessions  EvaluationSessionSet // nil ⇒ no session is an evaluation (operator approval applies)
	EgressAuditor EgressAuditor        // ADR-0043: records remote-tool data egress; nil ⇒ no auditing
	Retriever     ToolRetriever        // ADR-0044: relevance-ranks the granted menu; nil ⇒ full menu
	// Authz is the access-control decision point (ADR-0085). It gates the
	// data-store regime (Regime 1) and, once effects are declared, the effect
	// classes an invocation may exercise. nil ⇒ the OSS allow-all default.
	Authz           Authorizer
	ContentStore    ContentStore // nil ⇒ results returned inline
	InlineThreshold int          // results larger than this go to the ContentStore
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
	ArtifactBytes     ArtifactByteWriter                                 // nil ⇒ no durable vault promotion
	ArtifactMeta      ArtifactRecorder                                   // nil ⇒ no durable metadata record
	ArtifactTags      func(ctx context.Context, agentID string) []string // kernel-derived write tags; nil ⇒ none
	ArtifactOutputDir string                                             // "" ⇒ no on-disk materialization
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

// artifactSession returns the session an artifact produced by this call belongs to.
//
// The session is resolved server-side and carried on ctx (the transport derives it from the
// caller's unforgeable lease). The lease is used only as a last-resort correlation key when
// no session is in play — a tool call outside any session still produces a retrievable
// artifact, just one scoped to that call.
func artifactSession(ctx context.Context, req ToolCallRequest) string {
	if sid, ok := SessionIDFromContext(ctx); ok && sid != "" {
		return string(sid)
	}
	return string(req.SessionTokenID)
}

// budgetAccountFor keys a call's spend to its managed session (ADR-0043 D5).
func budgetAccountFor(req ToolCallRequest) string {
	return "mcp:" + string(req.SessionTokenID)
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

	// Tool resolution. A contributed local:<machine>/<tool> identity NEVER
	// resolves from the kernel-global registry: it resolves per call from the
	// task beneficiary's own live fleet (ADR-0127 D4), and this is also where
	// the dispatch-layer scoping holds — a machine outside the beneficiary's
	// fleet is refused HERE, structurally, whatever any menu said (the D9
	// defense in depth; the refusal is recorded as a decision). The two
	// namespaces cannot answer for the same name: the registry refuses local:
	// registrations at its chokepoint, and every contributed name carries the
	// prefix.
	var tool SystemTool
	var ok bool
	var contrib *contributedResolution
	if strings.HasPrefix(req.ToolName, LocalToolPrefix) {
		res, reason, resolved := e.resolveContributed(ctx, req)
		if !resolved {
			return denied(reason, argHash)
		}
		contrib = &res
		tool = res.tool
		// The ladder (D6) may have chosen the machine for a bare-capability
		// step: from here on the call IS the namespaced step it resolved to —
		// grants, effects, egress audit and dispatch all see the real target.
		req.ToolName = res.tool.Name
	} else {
		tool, ok = e.Registry.Get(req.ToolName)
		if !ok {
			return denied("unknown tool", argHash)
		}
	}

	// Grant (fail-closed on unknown principal / no grant). A2.2: an operator
	// ScopeSystem execution (req.System) bypasses the per-agent grant with an
	// allow-all policy — the operator is above the scope plane (D13).
	var grant ToolGrant
	if req.System {
		grant = ToolGrant{Tool: req.ToolName, Policy: ToolResourcePolicy{AllowAll: true}}
	} else {
		g, granted := e.grantFor(ctx, req.AgentID, req.ToolName, string(req.SessionTokenID))
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

	// Regime 3 — EFFECT classes (ADR-0086). The tag check above asks what the tool
	// is ABOUT; this asks what the invocation DOES. Both must pass, and this one
	// applies to every tool, not only the tagged-store ones — "no tool may
	// transmit outside this network" has to hold for a tool that touches no store.
	//
	// An operator ScopeSystem execution carries its own authority (A2.2) and is not
	// effect-gated; the resource-arg policy and process confinement still apply.
	if !req.System {
		if dec := e.effectDecision(ctx, req, tool); !dec.Allowed {
			return denied("effect not permitted: "+dec.Detail, argHash)
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

	// ADR-0127 CL-2: the contributed pre-dispatch gate — parking for an
	// offline target (D6), then the consent decision (D7). Deliberately AFTER
	// every kernel authorization above (a step the kernel would refuse anyway
	// must not prompt anyone) and immediately BEFORE dispatch, so a consent
	// approval approves what actually runs.
	if contrib != nil {
		resp, proceed := e.contributedGate(ctx, req, contrib, argHash)
		if !proceed {
			return resp
		}
		tool = contrib.tool // parking re-resolves against the fresh manifest
	}

	// Dispatch. ADR-0043: an mcp:<server>/<tool> identity routes to the MCP
	// handler; ADR-0127 D5: a local:<machine>/<tool> identity routes to the
	// worker relay; everything else runs as a confined native subprocess. All
	// three implement ToolHandler, so authorization above is identical — no
	// prefix ever changes what a call must pass, only who executes it.
	handler := e.Handler
	isMCP := strings.HasPrefix(req.ToolName, "mcp:")
	if isMCP && e.MCPHandler != nil {
		handler = e.MCPHandler
	}
	isLocal := strings.HasPrefix(req.ToolName, LocalToolPrefix)
	if isLocal {
		handler = e.LocalHandler // nil ⇒ "no tool handler configured" below
	}
	// ADR-0043 D4: a remote MCP call sends the agent's args outside the trust
	// boundary — record the egress (the call itself is allowed; the operator owns
	// endpoint trust, and Regime-1 above already enforced any declared data class).
	// A contributed call egresses too, unconditionally (ADR-0127, owner ruling
	// 2026-08-20): the arguments leave the deployment for the consumer's machine
	// even when the tool only reads.
	if (isMCP || isLocal) && e.EgressAuditor != nil {
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
		if contrib != nil && errors.Is(err, ErrWorkerConsentDenied) {
			// The machine itself declined the step (on-machine-only knob,
			// ADR-0127 D7): a recorded REFUSAL, not a worker error. The deny
			// text is kernel-authored; nothing worker-written rides it.
			e.recordFleetDecision(AccessDecision{
				Resource:  ResourceRef{Kind: KindTool, ID: req.ToolName},
				Principal: AgentPrincipal(req.AgentID),
				Surface:   SurfaceFromContext(ctx),
				Reason:    ReasonConsentDeniedOnMachine,
				Detail:    "machine " + strconv.Quote(contrib.machine) + " declined consent locally",
			})
			return denied("consent denied on machine "+strconv.Quote(contrib.machine), argHash)
		}
		if contrib != nil {
			// The relay itself failed: the machine stopped answering, the call
			// deadline passed, the transport broke. The step was consented and
			// dispatched, so the ONLY record of what became of it is this one —
			// a machine vanishing mid-step is the failure an operator most
			// needs to see, and it used to leave the journal at the consent
			// decision with nothing after it.
			e.recordFleetDecision(AccessDecision{
				Resource:  ResourceRef{Kind: KindTool, ID: req.ToolName},
				Principal: AgentPrincipal(req.AgentID),
				Surface:   SurfaceFromContext(ctx),
				Reason:    ReasonRelayFailed,
				Detail:    "machine " + strconv.Quote(contrib.machine) + ": " + err.Error(),
			})
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
	// Every non-denied call is REPORTED; the recorder decides what to keep. The size
	// floor is passed as FactEligible rather than applied here, because it answers only
	// "is this output worth embedding as a fact" — it must not also decide whether the
	// call's engaged entities reach the world model. A denied call is skipped outright:
	// it never ran, so it touched nothing.
	if e.ToolOutput != nil && !isDeniedResult(result) {
		isMutation := len(tool.DataWriteKinds) > 0
		_ = e.ToolOutput.RecordToolOutput(ctx, ToolOutputRecord{
			ToolName:   req.ToolName,
			ArgsJSON:   req.ArgsJSON,
			Output:     result,
			IsMutation: isMutation,
			TaskID:     req.TaskID,
			// A mutation is always durable history; a read must clear the cost floor
			// before it is curated as knowledge.
			FactEligible:       isMutation || shouldPromoteToolOutput(result, e.ToolOutputMinBytes),
			ClassificationTags: tool.AuthzTags(),
		})
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
					// Phase 3: the artifact belongs to the TASK SESSION, resolved
					// server-side from the caller's lease. It used to be stamped with the
					// per-step lease itself, so ListStepArtifacts(sessionID) could never
					// find it — the artifact was addressed by a key nothing would ever
					// look it up by.
					SessionID: string(artifactSession(ctx, req)),
					Tags:      tags,
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
	// ADR-0127 D4: the task beneficiary's contributed tools join the kernel
	// menu — resolved per task, appended here so the restricted-principal
	// ceiling below applies to them exactly as it does to kernel tools.
	menu = append(menu, e.contributedTools(ctx, agentID)...)
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
	// ADR-0051 D6: a restricted principal may use ONLY its allowlisted (discovery-safe)
	// tools — a hard ceiling enforced BEFORE the Unrestricted bypass, so it holds even in
	// dev/unrestricted mode. Everything else is denied fail-closed.
	//
	// RestrictedTools is empty in a current kernel: its only principal was the Scout,
	// retired 2026-08-07 with the config key that populated it. An absent entry means
	// "not a restricted principal", so an empty map is a no-op here rather than a
	// deny-all — the ceiling stays available for the next read-only organ.
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

// contributedTools resolves the task beneficiary's live fleet into menu
// entries (ADR-0127 D4). The resolution is STRUCTURAL: it starts from the
// beneficiary carried on ctx and asks the fleet for that owner's live workers
// — there is no global list of contributed tools anywhere to filter, so a
// forgotten filter cannot put one owner's filesystem in another owner's menu.
// No beneficiary, or no fleet source, resolves to nothing, fail-closed.
//
// Grant discipline mirrors grantFor so the advisory menu keeps matching what
// Execute will allow: under the Unrestricted bypass every resolved tool
// appears; otherwise only those the agent holds a grant for under the
// namespaced name. And a name the kernel registry already holds is skipped —
// a contributed tool can never displace a kernel tool (the registry refuses
// local: names at its chokepoint, so this is a second lock on a door that is
// already shut).
func (e *ToolExecutor) contributedTools(ctx context.Context, agentID string) []SystemTool {
	if e.Fleet == nil {
		return nil
	}
	owner := TaskBeneficiaryFromContext(ctx)
	if owner.IsZero() {
		return nil
	}
	var granted map[string]bool
	if !e.Unrestricted {
		grants, err := e.Grants.GrantsFor(ctx, agentID)
		if err != nil {
			return nil
		}
		granted = make(map[string]bool, len(grants))
		for _, g := range grants {
			granted[g.Tool] = true
		}
	}
	var out []SystemTool
	for _, w := range e.Fleet.LiveFleet(ctx, owner) {
		for _, t := range w.Tools {
			attached := AttachContributedTool(w, t)
			if _, shadow := e.Registry.Get(attached.Name); shadow {
				continue
			}
			if granted != nil && !granted[attached.Name] {
				continue
			}
			out = append(out, attached)
		}
	}
	return out
}

// contributedResolution is one resolved contributed step: the attached tool
// (its Name is the full local:<machine>/<tool> the rest of Execute uses), the
// worker's registration (the D7 consent knob rides it), and whether the
// machine is live right now — false means the pre-dispatch gate PARKS the step
// (ADR-0127 D6, CL-2).
type contributedResolution struct {
	tool    SystemTool
	reg     WorkerRegistration
	machine string
	bare    string
	live    bool
}

// bareContributedCapability recognises local:<capability> — a contributed call
// naming a capability but NO machine, which the selection ladder resolves.
func bareContributedCapability(name string) (string, bool) {
	rest, found := strings.CutPrefix(name, LocalToolPrefix)
	if !found || rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	return rest, true
}

// resolveContributed is the dispatch-side resolution of a contributed call —
// and the SECOND scoping layer, independent of menu construction: the
// machine's registered owner must BE the task's beneficiary (ADR-0127 D1,
// applied again at dispatch per D9's defense in depth). A cross-owner step is
// refused with the refusal recorded as a decision; an unknown machine, an
// absent fleet and a task with no beneficiary refuse identically, so a caller
// cannot probe whose fleet a machine is in. Ownership holds for EVERY caller —
// even an operator ScopeSystem execution cannot reach a stranger's machine,
// because the authority being exercised belongs to the machine's owner, not to
// the kernel.
//
// CL-2 grows two branches on the CL-0 core:
//
//   - a name with no machine segment (local:<capability>) resolves through the
//     D6 selection ladder (resolveByLadder) — explicit naming is rung 1 and is
//     this function's ordinary path;
//   - an owned machine that is REGISTERED but not live resolves live=false
//     when the fleet can park (WorkerRegistry + LivenessWaiter — the hub);
//     the pre-dispatch gate parks it with a deadline. A fleet that cannot park
//     keeps the CL-0 refusal.
//
// ok=false returns the deny reason; refusals also flow to recordFleetDecision.
func (e *ToolExecutor) resolveContributed(ctx context.Context, req ToolCallRequest) (contributedResolution, string, bool) {
	machine, bare, wellFormed := SplitContributedToolName(req.ToolName)
	if !wellFormed {
		if capability, isBare := bareContributedCapability(req.ToolName); isBare {
			return e.resolveByLadder(ctx, req, capability)
		}
		return contributedResolution{}, "unknown tool", false
	}
	beneficiary := TaskBeneficiaryFromContext(ctx)
	owned := false
	if e.Fleet != nil && !beneficiary.IsZero() {
		if owner, known := e.Fleet.OwnerOf(ctx, machine); known && owner == beneficiary {
			owned = true
		}
	}
	if !owned {
		e.recordFleetDecision(AccessDecision{
			Resource:  ResourceRef{Kind: KindTool, ID: req.ToolName},
			Principal: AgentPrincipal(req.AgentID),
			Surface:   SurfaceFromContext(ctx),
			Reason:    ReasonWorkerNotOwned,
			Detail:    "machine " + strconv.Quote(machine) + " is not in the task beneficiary's fleet",
		})
		return contributedResolution{}, "worker not owned: machine " + strconv.Quote(machine) +
			" is not in the task beneficiary's fleet", false
	}
	// Owned — live and offering the tool is the ordinary dispatch (D6: only a
	// live worker serves a step NOW).
	for _, w := range e.Fleet.LiveFleet(ctx, beneficiary) {
		if w.Machine != machine {
			continue
		}
		t, offers, ambiguous := ManifestToolFor(w, bare)
		if ambiguous {
			// CL-3: the name arrived as a capability TAG that collapses two
			// different real tools on this machine. Never pick one of two —
			// either exact wire name still resolves outright.
			return contributedResolution{}, "contributed tool unavailable: " + strconv.Quote(bare) +
				" names more than one tool on machine " + strconv.Quote(machine) + "; call the tool by its exact name", false
		}
		if offers {
			// bare becomes the REAL wire name (an exact match is itself; a tag
			// resolves to the one tool it names), so dispatch always keys on
			// what the local server published.
			return contributedResolution{tool: AttachContributedTool(w, t), reg: w, machine: machine, bare: t.Name, live: true}, "", true
		}
	}
	// Owned but not live: PARK when the fleet supports it (CL-2, D6) — the
	// last-offered manifest must still carry the tool, and the source must be
	// able to wait for liveness. Otherwise the CL-0/CL-1 refusal stands.
	if wr, hasReg := e.Fleet.(WorkerRegistry); hasReg {
		if _, canWait := e.Fleet.(LivenessWaiter); canWait {
			if reg, known := wr.RegistrationOf(ctx, machine); known {
				if t, offers := manifestTool(reg, bare); offers {
					return contributedResolution{tool: AttachContributedTool(reg, t), reg: reg, machine: machine, bare: t.Name, live: false}, "", true
				}
			}
		}
	}
	return contributedResolution{}, "contributed tool unavailable: machine " + strconv.Quote(machine) +
		" is not live or does not offer it", false
}

// targeting builds the CL-3 owner-scoped targeting layer from this executor.
// It is the SAME object a routing/matching layer resolves through, so the D6
// ladder and the D9 ownership restriction exist exactly once.
func (e *ToolExecutor) targeting() WorkerTargeting {
	return WorkerTargeting{Fleet: e.Fleet, Consent: e.Consent, Decisions: e.recordFleetDecision}
}

// ContributedVocabulary is the seam a routing/matching layer reads the task's
// contributed capability vocabulary through (ADR-0127 D9, CL-3): normalized,
// owner-tagged, and resolved through the SAME owner-scoped resolution the menu
// uses — so routing can never see, let alone target, a capability outside the
// task beneficiary's own fleet. No beneficiary ⇒ nothing, fail-closed.
func (e *ToolExecutor) ContributedVocabulary(ctx context.Context) []ContributedCapability {
	return e.targeting().Vocabulary(ctx)
}

// resolveByLadder resolves a bare local:<capability> step by asking the
// targeting layer which machine serves it (ADR-0127 D6/D9). The rungs live in
// WorkerTargeting.Target and NOWHERE else: dispatch and routing ask the same
// function, so "prefer the default machine rather than prompting" cannot mean
// two different things in two places.
func (e *ToolExecutor) resolveByLadder(ctx context.Context, req ToolCallRequest, capability string) (contributedResolution, string, bool) {
	tgt, reason, ok := e.targeting().Target(ctx, WorkerSelection{
		Capability:    capability,
		AgentID:       req.AgentID,
		TaskID:        taskIDOf(ctx, req),
		ArgsJSON:      req.ArgsJSON,
		RequestedName: req.ToolName,
	})
	if !ok {
		return contributedResolution{}, reason, false
	}
	// bare is the REAL wire name the target resolved to, never the capability
	// tag: the broker calls its local server by the name that server published.
	return contributedResolution{
		tool:    AttachContributedTool(tgt.Registration, tgt.Definition),
		reg:     tgt.Registration,
		machine: tgt.Machine,
		bare:    tgt.Tool,
		live:    tgt.Live,
	}, "", true
}

// consentArgsPreview bounds the raw args surfaced on a prompt.
const consentArgsPreview = 500

// defaultParkDeadline bounds a parked step when mcp.worker_park_deadline_ms is
// unset: 24h — long enough for a laptop asleep overnight, bounded enough that
// the failure is seen the next day, not never.
const defaultParkDeadline = 24 * time.Hour

// parkDeadline returns the configured park window, defaulting to 24h.
func (e *ToolExecutor) parkDeadline() time.Duration {
	if e.ParkDeadline > 0 {
		return e.ParkDeadline
	}
	return defaultParkDeadline
}

// taskIDOf correlates a prompt/decision with the task: the session id the
// kernel resolved from the caller's lease, else the per-step correlation key.
func taskIDOf(ctx context.Context, req ToolCallRequest) string {
	if sid, ok := SessionIDFromContext(ctx); ok && sid != "" {
		return string(sid)
	}
	return req.TaskID
}

// objectOfArgs surfaces the exact object a step acts on where TRIVIALLY
// derivable — the raw top-level "path" (or "url") string argument, verbatim.
// Deliberately not semantic parsing: everything else stays in the prompt's raw
// ArgsJSON.
func objectOfArgs(argsJSON []byte) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(argsJSON, &m) != nil {
		return ""
	}
	for _, k := range []string{"path", "url"} {
		raw, present := m[k]
		if !present {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			return s
		}
	}
	return ""
}

// contributedGate is the CL-2 pre-dispatch gate for a resolved contributed
// step: PARKING for an offline target (D6), then the CONSENT decision (D7).
// proceed=false returns the response to answer with. Every outcome — parked /
// dispatched-from-park / expired / abandoned; consent auto / approved / denied
// / timeout / unroutable / on-machine — lands on the decision seam under its
// own reason. Fail-closed throughout: no affirmative consent path, no dispatch.
func (e *ToolExecutor) contributedGate(ctx context.Context, req ToolCallRequest, c *contributedResolution, argHash string) (ToolCallResponse, bool) {
	record := func(allowed bool, reason DecisionReason, detail string) {
		e.recordFleetDecision(AccessDecision{
			Allowed:   allowed,
			Resource:  ResourceRef{Kind: KindTool, ID: req.ToolName},
			Principal: AgentPrincipal(req.AgentID),
			Surface:   SurfaceFromContext(ctx),
			Reason:    reason,
			Detail:    detail,
		})
	}

	// ── D6 parking: the target was already offline when the step arrived.
	// (A machine that drops MID-step is the hub call deadline's business —
	// CL-1 — and is deliberately not handled here.)
	if !c.live {
		waiter, canWait := e.Fleet.(LivenessWaiter)
		if !canWait {
			// Resolution only marks a step parked when the fleet can wait;
			// defensive fail-closed all the same.
			return denied("contributed tool unavailable: machine "+strconv.Quote(c.machine)+" is not live", argHash), false
		}
		wait := e.parkDeadline()
		until := time.Now().Add(wait).UTC().Format(time.RFC3339)
		record(true, ReasonStepParked, "machine "+strconv.Quote(c.machine)+" offline — queued until "+until)
		if e.Consent != nil {
			e.Consent.Notify(ctx, ConsentPrompt{
				Kind:           ConsentPromptNotice,
				Machine:        c.machine,
				Tool:           req.ToolName,
				TaskID:         taskIDOf(ctx, req),
				AgentID:        req.AgentID,
				Beneficiary:    TaskBeneficiaryFromContext(ctx),
				ConversationID: ConversationIDFromContext(ctx),
				Notice:         "machine " + c.machine + " offline — queued until " + until,
			})
		}
		switch err := waiter.AwaitLive(ctx, c.machine, wait); {
		case err == nil:
			// The machine is back — fall through to re-resolution below.
		case errors.Is(err, ErrParkExpired):
			record(false, ReasonParkExpired, "machine "+strconv.Quote(c.machine)+" stayed offline past "+wait.String())
			return ToolCallResponse{
				Error:   "parked step expired: machine " + strconv.Quote(c.machine) + " stayed offline past " + wait.String() + " (ADR-0127 D6)",
				ArgHash: argHash,
			}, false
		default:
			record(false, ReasonParkAbandoned, "caller gave up while parked: "+err.Error())
			return ToolCallResponse{
				Error:   "parked step abandoned before machine " + strconv.Quote(c.machine) + " returned: " + err.Error(),
				ArgHash: argHash,
			}, false
		}
		// Re-resolve against the FRESH manifest — the poll that woke us
		// re-registered the worker — so consent judges what will run NOW, and
		// the D1 ownership invariant is re-checked at dispatch time (a rotate
		// --owner while parked re-points the machine).
		wr, hasReg := e.Fleet.(WorkerRegistry)
		if !hasReg {
			return denied("contributed tool unavailable: machine "+strconv.Quote(c.machine)+" returned without a readable registration", argHash), false
		}
		reg, known := wr.RegistrationOf(ctx, c.machine)
		t, offers := manifestTool(reg, c.bare)
		if !known || !offers {
			return denied("contributed tool unavailable: machine "+strconv.Quote(c.machine)+" no longer offers "+strconv.Quote(c.bare), argHash), false
		}
		if reg.Owner != TaskBeneficiaryFromContext(ctx) {
			record(false, ReasonWorkerNotOwned, "machine "+strconv.Quote(c.machine)+" is not in the task beneficiary's fleet")
			return denied("worker not owned: machine "+strconv.Quote(c.machine)+" is not in the task beneficiary's fleet", argHash), false
		}
		c.reg, c.tool, c.live = reg, AttachContributedTool(reg, t), true
		record(true, ReasonParkDispatched, "machine "+strconv.Quote(c.machine)+" returned; dispatching through the consent-checked path")
	}

	// ── D7 consent: knob × effect class → outcome.
	knob := c.reg.Consent
	if knob == "" {
		knob = ConsentAuto
	}
	effectful := ContributedStepEffectful(c.tool.Effects)
	switch {
	case knob == ConsentOnMachineOnly:
		// Strictest knob: the broker prompts locally for EVERY step; the wire
		// step carries the consent marker (hub-side) and a consent-denied
		// report is a recorded refusal.
		record(true, ReasonConsentOnMachine, "broker prompts locally; a consent-denied report is a recorded refusal")
		return ToolCallResponse{}, true
	case knob == ConsentAuto && !effectful:
		// The sealed default: reads run silently but receipted.
		record(true, ReasonConsentAuto, "read-only step under the auto knob")
		return ToolCallResponse{}, true
	}
	// any-surface — or effectful under auto, which the sealed ruling treats as
	// any-surface: approval is the default for anything effectful.
	if e.Consent == nil {
		record(false, ReasonConsentUnroutable, "consent required but no consent channel is wired (fail-closed)")
		return denied("consent required for "+req.ToolName+" but no consent channel is wired (fail-closed)", argHash), false
	}
	ans, outcome, err := e.Consent.Request(ctx, ConsentPrompt{
		Kind:           ConsentPromptApprove,
		Machine:        c.machine,
		Tool:           req.ToolName,
		Object:         objectOfArgs(req.ArgsJSON),
		ArgsJSON:       preview(req.ArgsJSON, consentArgsPreview),
		TaskID:         taskIDOf(ctx, req),
		AgentID:        req.AgentID,
		Beneficiary:    TaskBeneficiaryFromContext(ctx),
		ConversationID: ConversationIDFromContext(ctx),
	})
	switch {
	case err != nil:
		record(false, ReasonConsentTimeout, "consent request aborted: "+err.Error())
		return denied("consent not obtained for "+req.ToolName+": "+err.Error(), argHash), false
	case outcome == ConsentNoSubscriber:
		record(false, ReasonConsentUnroutable, "no surface is subscribed to answer (fail-closed)")
		return denied("consent required for "+req.ToolName+" but no surface is listening (fail-closed)", argHash), false
	case outcome == ConsentTimedOut:
		record(false, ReasonConsentTimeout, "consent request went unanswered (fail-closed)")
		return denied("consent request for "+req.ToolName+" timed out (fail-closed)", argHash), false
	case !ans.Approved:
		record(false, ReasonConsentDenied, "denied by "+ans.AnsweredBy)
		return denied("consent denied for "+req.ToolName+" on machine "+strconv.Quote(c.machine), argHash), false
	}
	record(true, ReasonConsentApproved, "approved by "+ans.AnsweredBy)
	return ToolCallResponse{}, true
}

// recordFleetDecision makes a contributed-lane decision durable-adjacent:
// always logged (allows at info, refusals at warn), and handed to the
// FleetDecisions seam when one is wired (tests; the premium decision journal).
func (e *ToolExecutor) recordFleetDecision(dec AccessDecision) {
	if dec.Allowed {
		slog.Info("ADR-0127: contributed-step decision", "decision", dec.Explain())
	} else {
		slog.Warn("ADR-0127: contributed-step refused", "decision", dec.Explain())
	}
	if e.FleetDecisions != nil {
		e.FleetDecisions(dec)
	}
}

// ConferSkillGrants activates a loaded system skill's tool grants run-scoped
// (ADR-0046 D6), INTERSECTED with what policy permits the principal (ADR-0085 D4).
//
// This is the security-critical half of the skill model. A skill is gated on
// retrieval by its tags, and loading it activates tool grants — so without an
// intersection, "if you can see the skill you get its tools" makes skill
// visibility a privilege-granting operation, and the skill tag vocabulary
// silently becomes a tool-permission vocabulary.
//
// The intersection is against POLICY, not against the agent's static grants: a
// system skill is operator-authored and may still confer a tool the agent has no
// standing grant for (that is D6, and it is the point of skills). What it can
// never do is confer a tool the decision point refuses — loading a skill only
// ever narrows or maintains privilege, exactly as a restricted Windows token
// cannot be widened by the thing it loads.
//
// A clipped grant does NOT fail the load: the rest of the skill activates and a
// ReasonSkillGrantClipped decision is returned per dropped tool. Denying the whole
// skill makes the system feel broken; granting the tool is a security hole.
// Returns the granted set and one decision per clip.
func (e *ToolExecutor) ConferSkillGrants(ctx context.Context, session, agentID string, tools []string) (granted []string, clipped []AccessDecision) {
	if len(tools) == 0 {
		return nil, nil
	}
	a := e.Authz
	if a == nil {
		a = AllowAllAuthorizer{}
	}
	for _, name := range tools {
		tool, known := e.Registry.Get(name)
		if !known {
			clipped = append(clipped, AccessDecision{
				Resource:  ResourceRef{Kind: KindTool, ID: name},
				Principal: AgentPrincipal(agentID),
				Reason:    ReasonSkillGrantClipped,
				Detail:    "skill grants an unknown tool",
			})
			continue
		}
		// ADR-0051 D6: a restricted principal's hard ceiling outranks a skill —
		// otherwise a skill would be the way around the Scout's confinement.
		if allow, restricted := e.RestrictedTools[agentID]; restricted && !allow[name] {
			clipped = append(clipped, AccessDecision{
				Resource:  tool.AuthzRef(),
				Principal: AgentPrincipal(agentID),
				Reason:    ReasonSkillGrantClipped,
				Detail:    "outside the principal's restricted tool ceiling",
			})
			continue
		}
		dec := a.Authorize(ctx, AccessRequest{
			Principal: AgentPrincipal(agentID),
			Surface:   SurfaceFromContext(ctx),
			Resource:  tool.AuthzRef(),
			Tags:      tool.AuthzTags(),
			Effects:   tool.Effects,
			Session:   SessionID(session),
		})
		if !dec.Allowed {
			dec.Reason = ReasonSkillGrantClipped
			if dec.Detail == "" {
				dec.Detail = "not permitted to this principal"
			}
			clipped = append(clipped, dec)
			continue
		}
		granted = append(granted, name)
	}
	e.Overlay.Activate(session, granted)
	return granted, clipped
}

// effectDecision asks the decision point whether every effect this tool declares
// is permitted to this principal on this surface. With no decision point (OSS) the
// answer is always yes — the check runs, the policy is simply empty.
func (e *ToolExecutor) effectDecision(ctx context.Context, req ToolCallRequest, tool SystemTool) AccessDecision {
	if len(tool.Effects) == 0 {
		// Unclassified only reaches here in non-strict mode, where registration
		// inferred a set. An empty set at this point means the tool bypassed
		// validation entirely, which is a wiring bug — deny and say so.
		return AccessDecision{
			Allowed: false, Reason: ReasonEffectNotPermitted,
			Detail: "tool declares no effect classes (unvalidated registration)",
		}
	}
	a := e.Authz
	if a == nil {
		a = AllowAllAuthorizer{}
	}
	return a.Authorize(ctx, AccessRequest{
		Principal: AgentPrincipal(req.AgentID),
		Surface:   SurfaceFromContext(ctx),
		Resource:  tool.AuthzRef(),
		Tags:      tool.AuthzTags(),
		Effects:   tool.Effects,
		Session:   SessionID(req.SessionTokenID),
	})
}

// scopeAllows applies the data-store regime (ADR-0039 D8 Regime 1): a tool that
// touches tagged stores may run only if the principal's predicate admits the tag
// classes the tool declares. The decision point resolves the predicate; a nil
// predicate (the plugin could not resolve the principal) denies.
func (e *ToolExecutor) scopeAllows(ctx context.Context, agentID string, tool SystemTool) bool {
	a := e.Authz
	if a == nil {
		a = AllowAllAuthorizer{}
	}
	eff, _ := a.ReadFilter(ctx, AgentPrincipal(agentID), SurfaceFromContext(ctx))
	if eff == nil {
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
