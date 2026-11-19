package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"
)

// ToolOutputRecorder feeds a successful tool output into the memory layer. Satisfied
// by the MemoryAgent, whose RecordToolOutput writes nothing since ENG-1595 (owner
// decision 2026-11-14): the `mnemonic_action` action records it used to write, and
// the action lane that read them (removed with ENG-1645), wait for the owner-led
// redesign of agent-experience memory. The hook stays so the tool path needs no
// change when that lands.
type ToolOutputRecorder interface {
	RecordToolOutput(ctx context.Context, rec ToolOutputRecord) error
}

// ToolOutputRecord carries one tool call's output to the recorder. IsMutation (the
// tool has DataWriteKinds — ADR-0034) marks an action record; ArgsJSON is needed to
// format the action line ("what was done where").
type ToolOutputRecord struct {
	ToolName   string
	ArgsJSON   []byte
	Output     []byte
	IsMutation bool
	TaskID     string // ADR-0049 D3: per-step correlation key stamped on action records
	// ClassificationTags are the tool's own domain tags (AuthzTags), carried so the
	// LTM write is classified from principal + tool domain. The pre-invocation gates
	// already consume these; the artifact path already classifies its writes — this
	// closes the one destination that landed untagged.
	ClassificationTags []string
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

// isDeniedResult reports whether a tool result is a denial (the call did not run).
func isDeniedResult(result []byte) bool {
	var obj map[string]json.RawMessage
	if json.Unmarshal(result, &obj) == nil {
		_, ok := obj["denied"]
		return ok
	}
	return false
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
	// Carried so tool spend is attributed to the step's lease (priceFor). Empty
	// for callers that do not run under a managed session. (It once also flagged
	// evaluation sessions of the ADR-0037 interview, deleted 2026-11-05.)
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
	ResultCID  string // the result's reference: offloaded (ResultJSON nil), or inline plus its ledger id
	// Offloaded summarises a result that travels as its reference (ResultJSON is
	// nil then): size, content type, shape, preview and how to read it. It is what
	// the AGENT is shown — ExecuteTool puts its JSON in result_json beside
	// result_cid — while kernel consumers (the stager, the plan action ledger) keep
	// reading ResultJSON == nil as "read the entry".
	Offloaded  *OffloadSummary
	Denied     bool
	DenyReason string
	Error      string
	ArgHash    string
	ResultHash string
	ApproverID string
	// ApprovalState is the approval's resolution when the call was refused at the
	// approval gate (denied, expired, unroutable, needs_hierarchy, ...); "" otherwise.
	// Structured so a consumer (the per-run unroutable bound in ExecuteTool) never has
	// to parse DenyReason.
	ApprovalState ApprovalState
	// SourceClass labels the content of a result the call returned (ENG-531,
	// source_class.go): web for a tool that reaches outside, tool otherwise, a
	// kernel tool's own class. "" when nothing reached the agent (a refusal).
	SourceClass SourceClass
	// Annotations is the context pre_tool / post_tool hooks added (ADR-0139 D11),
	// delivered to the agent BESIDE the result, never inside ResultJSON, so ledger
	// records and replayed results stay byte-identical (owner decision 2026-11-15,
	// T-20261105-pre-tool-and-post-tool-hook-annotations-are).
	Annotations []string
	// Untrusted marks a response whose result (and tool-reported error) is
	// untrusted data — every MCP response (ADR-0043 D4, ENG-117) — with its
	// injection-scan verdict. The agent-facing boundary fences it
	// (AgentToolResult); ResultJSON stays raw for kernel consumers.
	Untrusted *UntrustedOutput
}

// ToolExecutor is the single reference monitor for tool execution (ADR-0039 D4).
// It authorizes (grant + resource policy + data scope + approval) entirely
// kernel-side and pre-invocation (A1.4), then dispatches to the handler, audits,
// and offloads large results. It never panics.
type ToolExecutor struct {
	Registry ToolRegistry
	Grants   GrantsProvider
	// Handler is the default handler for an identity that is neither mcp: nor
	// local:. nil in production since ENG-1421 deleted the native tool.py
	// process handler (MCP tools only, decisions/agent-stack-v2-mcp-tools-and-profiles.md),
	// so such a call is refused "no tool handler configured".
	Handler    ToolHandler
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
	approval      ApprovalController // nil ⇒ dangerous tools are denied (fail-closed)
	EgressAuditor EgressAuditor      // ADR-0043: records remote-tool data egress; nil ⇒ no auditing
	Retriever     ToolRetriever      // ADR-0044: relevance-ranks the granted menu; nil ⇒ full menu
	// authz is the access-control decision point (ADR-0085). It gates the
	// data-store regime (Regime 1) and, once effects are declared, the effect
	// classes an invocation may exercise. Required by NewToolExecutor (the OSS
	// default is AllowAllAuthorizer{}, passed explicitly); the nil ⇒ allow-all
	// fallback in the readers is reachable only from a struct literal inside
	// this package's tests.
	//
	// The security controls (approval, authz, unrestricted, ceiling,
	// leaseCeiling, argValidator, idempotency, guards, writeTargets, subtasks)
	// are unexported: NewToolExecutor is the only way to set them, so none can
	// be attached or swapped after construction (AGENTS.md §2.3,
	// T-20261113-domain-toolexecutor-has-no-constructor-its).
	authz        Authorizer
	ContentStore ContentStore // nil ⇒ results returned inline
	// InlineThreshold: a result larger than this travels as its reference plus an
	// OffloadSummary (execution.content.inline_result_bytes). 0 ⇒ always inline.
	InlineThreshold int
	// PreviewChars bounds an offload summary's preview, and a result longer than
	// it carries its ledger reference even inline (execution.content.preview_chars;
	// 0 ⇒ 600). PageBytes is ledger_get's page, for the summary's page count
	// (execution.content.page_bytes; 0 ⇒ one page).
	PreviewChars int
	PageBytes    int
	// ADR-0043 budget regime: when both are set, a priced tool call is reserved
	// against the session budget before dispatch and reconciled to actual after.
	// nil ⇒ tool calls are unmetered (no behaviour change).
	Budget  *BudgetLedger
	Pricing ToolPricingSource
	// unrestricted is the operator-chosen bypass (ExecutionConfig.ToolsUnrestricted):
	// every named agent may call every registered tool with an allow-all resource
	// policy. Approval for dangerous tools STILL applies; an anonymous principal is
	// still denied. Trusted/dev deployments only.
	unrestricted bool
	// freeQueryOpen lifts the free-query guard (ToolExecutorControls.FreeQueryOpen,
	// ENG-2232). false — the zero value and the default — means a FreeQuery tool
	// needs a grant naming it exactly (freeQueryGuarded, grantFor).
	freeQueryOpen bool
	// RestrictedTools (ADR-0051 D6) caps which tools a principal may use, as a HARD
	// CEILING that overrides Unrestricted: principal id → the set of tool names it may
	// touch. A principal absent from the map is unrestricted (normal resolution). A
	// principal present may use ONLY its listed tools, even under the Unrestricted bypass
	// — fail-closed for everything else. Its first principal was the ADR-0051 Scout
	// (confined to the operator's `discovery-safe` set); the Scout and the config key
	// that populated this map were removed 2026-08-07, so no production code sets it
	// today. It is kept on purpose (owner, 2026-08-10) as the generic ceiling for the
	// next unattended read-only organ; the executor knows nothing of any specific one.
	RestrictedTools map[string]map[string]bool
	// ceiling is the glob-based twin of RestrictedTools: a declarative agent
	// profile's tool allowlist (agents/profiles/README.md), enforced as a HARD
	// ceiling at every point RestrictedTools is — before the Unrestricted bypass and
	// before the skill overlay — so a profile agent stays inside its allowlist even
	// under tools_unrestricted and a loaded skill cannot widen it (agent stack v2,
	// T-20261105-agent-stack-v2-worker-agent-runtime-plus). nil ⇒ no profile ceiling.
	// A tool must be inside BOTH ceilings (withinCeiling).
	ceiling ToolCeiling
	// Overlay holds run-scoped grants conferred by a loaded system skill (ADR-0046
	// D6). nil ⇒ no skill-conferred grants. Consulted in grantFor after the static
	// grants; dangerous tools still require approval regardless of how granted.
	Overlay *RunGrantOverlay
	// ToolOutput receives every non-denied tool output (ADR-0049 D1: a mutation
	// becomes an action record). nil ⇒ nothing is recorded.
	ToolOutput ToolOutputRecorder
	// Hooks is the ADR-0139 D11 lifecycle-hook chain: pre_tool runs after the
	// policy decision and can only narrow it; post_tool may rewrite or withhold
	// the output. nil ⇒ no hooks (behaviour unchanged).
	Hooks *HookChain
	// History is the conversation-scoped record of side-effecting calls the
	// executor keeps, and matches every approval it raises against, so the
	// approver is told of a possible duplicate (action_history.go;
	// execution.tools.duplicate_notice_window_minutes). nil: no record, no notice.
	History *ActionHistory
	// leaseCeiling is the ADR-0139 D12 per-LEASE tool ceiling: a subagent's
	// toolset (a subset of its parent's tools, resolved by the kernel at spawn)
	// bound to the child's lease. Checked wherever the session token is known,
	// after the per-agent ceilings and before the Unrestricted bypass and the
	// skill overlay, so neither can widen a child past its toolset. nil ⇒ none.
	leaseCeiling LeaseToolCeiling
	// Ledger is the ADR-0145 D1 run ledger's writer: every result of a call that
	// ran (read or write, success or handler error) is recorded here after the
	// post_tool hooks, so the ledger holds what the agent was actually given.
	// When set it also REPLACES the content-store offload of large results: a
	// result over InlineThreshold is returned as its ledger id (ResultCID "led_…",
	// readable through ledger_get and GetContextNode) instead of a plan-scoped
	// content-store cid. nil ⇒ the legacy path (execution.ledger.enabled off).
	Ledger LedgerWriter
	// KernelTools are the tools the kernel serves in process (domain.KernelTool:
	// ledger_get / ledger_find). They resolve before the registry, need no grant
	// (they read only what the caller's predicate admits), and are never recorded
	// to the ledger themselves. nil ⇒ none.
	KernelTools []KernelTool

	// ADR-0145 D9 executor guards (T-20261108-adr-0145-phase-2-requirement-checklist-with),
	// each nil/false ⇒ off (the legacy behaviour). The D9 per-step effect menu was
	// removed by ADR-0145 D14: there is no per-step tool permission.
	//
	// argValidator validates and coerces arguments against the tool's schema
	// before authorization (execution.tools.validate_args); an invalid call
	// returns an Error the model can correct, and never runs.
	argValidator ToolArgValidator
	// idempotency stores side-effecting results by IdempotencyKey(run, step,
	// tool, args) (execution.tools.idempotent_writes): a retry gets the stored
	// result instead of running the write again.
	idempotency *IdempotencyCache
	// ReturnLedgerRef returns every recorded result's ledger id in ResultCID even
	// when the result is small enough to travel inline (ResultJSON is kept), so a
	// worker can cite the entry a value came from in its answer envelope (ADR-0145
	// D6). Set with execution.plan.checklist_enabled; the SDK reads a response with
	// both fields as an inline result plus its reference.
	ReturnLedgerRef bool
	// guards are the ADR-0145 D14 request-tied guards (execution.tools.request_guards):
	// the checklist's constraints by tool class, the per-request write-target
	// record, the repair-turn write guard, approval with a notice for a write no
	// open write/document item asked for, and the per-step record of refused calls.
	// nil ⇒ none.
	guards *RequestGuards
	// writeTargets derives each side-effecting call's target from its arguments
	// (execution.tools.write_targets). It is the per-target effect record the plan
	// action ledger keeps for every write (PlanAction.Target), which coverage
	// matches a write item's target against (ADR-0145 D5.3/D14,
	// T-20261112-coverage-settles-a-write-item-on-any-done). Independent of Guards:
	// the record is always kept; Guards.Targets is the same rule set when the
	// request guards are on. nil ⇒ actions carry no target.
	writeTargets *WriteTargets
	// Stager stages the inputs of a tool that declares StagedInputs (the code
	// sandbox) into a per-run read-only folder, resolved under the caller's
	// access (domain/staged_inputs.go; execution.tools.staged_inputs). nil ⇒ a
	// staged tool called WITH inputs is refused (never run without them).
	Stager ToolInputStager
	// StepFailures counts each step lease's identical failed calls (tool, args,
	// error) and stops the step at execution.tools.repeated_failure_stop
	// (step_failures.go). nil ⇒ off.
	StepFailures *StepFailures
	// subtasks is the ADR-0150 registry of subtask parents and child leases
	// (domain/subtasks.go). On a child lease the executor enforces the lease's
	// EFFECT ceiling itself — independent of Authz, which is AllowAllAuthorizer in
	// OSS — keys every side-effecting call deterministically (a resumed subtask
	// never repeats a write), labels an approval with the subtask, and records
	// what the worker did (evidence, read-the-web). nil ⇒ no subtasks.
	subtasks *Subtasks
	// outboundIdentifiers is the outbound identifier guard (ENG-917,
	// domain/outbound_identifiers.go; execution.tools.outbound_identifier_guard):
	// a call that can send outside whose text arguments carry a personal
	// identifier goes to approval, the identifiers named in the request. nil ⇒ off.
	outboundIdentifiers *OutboundIdentifierGuard
	// untrustedOutput scans every MCP response for injection patterns and says
	// what a hit does (ADR-0043 D4, ENG-117; execution.tools.output_scan). nil ⇒
	// MCP responses are still marked untrusted and fenced, but not scanned.
	untrustedOutput *UntrustedOutputGuard
	// ruleOfTwo is the Rule-of-Two gate (ENG-531, domain/rule_of_two.go;
	// execution.tools.rule_of_two): a gated (write/egress) call from a session
	// that has consumed untrusted content and holds sensitive access goes to
	// approval, what tainted it named in the request. It also tracks what each
	// session consumed: every tool result here, every recall through
	// ObserveContent. nil ⇒ off.
	ruleOfTwo *RuleOfTwoGate
}

// observeFailure counts a failed call (a tool error, or a final refusal) against
// its step lease and adds the hint or the stop notice to what the worker is told.
// A call that succeeded, or a refusal still waiting on a person, is not a failure.
func (e *ToolExecutor) observeFailure(lease LeaseID, tool string, args []byte, out *ToolCallResponse) {
	var errText string
	switch {
	case out.Error != "":
		errText = out.Error
	case out.Denied && refusalIsFinal(out.ApprovalState):
		// The D14.5 refusal record answers a repeat with a note appended: the
		// failure is the same one.
		errText = strings.TrimSuffix(out.DenyReason, RepeatedRefusalNote)
	default:
		return
	}
	count, stuck := e.StepFailures.Observe(lease, tool, args, errText)
	var note string
	switch {
	case stuck:
		note = fmt.Sprintf(RepeatedFailureStop, count)
		slog.Warn("step stopped: identical tool failure repeated (execution.tools.repeated_failure_stop)",
			"tool", tool, "count", count, "lease", lease, "err", errText)
	case count >= 2 && count == e.StepFailures.Threshold()-1:
		note = fmt.Sprintf(RepeatedFailureHint, count)
	default:
		return
	}
	if out.Error != "" {
		out.Error += note
	} else {
		out.DenyReason += note
	}
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
	return "mcp:" + string(req.SessionTokenID)
}

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

// Execute authorizes and runs one tool call — the single chokepoint every tool
// call passes (ADR-0039). The stages run in order (card.core.tools-mcp):
//
//  1. step failures — a stopped step runs nothing more;
//  2. kernel tools  — ledger_get/ledger_find and the other kernel-served tools;
//  3. refusal record — an identical call this step had refused is answered from it;
//  4. resolve       — the tool (registry, or the beneficiary's fleet) and its arguments;
//  5. write keys    — the subtask effect ceiling and keys, the action key, step idempotency;
//  6. authorize     — grant, CID hydration, resource policy, scope, effects, request
//     guards, staged inputs;
//  7. policy        — the verdict, pre_tool hooks, the outbound identifier guard, the
//     Rule-of-Two gate;
//  8. approval      — the approval gate, or the recovered approval;
//  9. contributed   — the ADR-0127 parking and consent gate;
//  10. dispatch     — the handler, egress audit, budget, the call;
//  11. result       — staged outputs, binary blocks, post_tool hooks, the untrusted-output
//     scan, memory, the ledger and offload.
//
// A call refused by any stage before dispatch never reaches the handler.
func (e *ToolExecutor) Execute(ctx context.Context, req ToolCallRequest) (out ToolCallResponse) {
	c := &toolCall{req: req, argHash: hashBytes(req.ArgsJSON)}

	resp, stop, fin := e.stepFailureStage(ctx, c)
	for _, f := range fin {
		defer f(&out)
	}
	if stop {
		return resp
	}

	if kt := e.kernelTool(c.req.ToolName); kt != nil {
		return e.runKernelTool(ctx, c.req, kt, c.argHash)
	}

	resp, stop, fin = e.refusalRecordStage(ctx, c)
	for _, f := range fin {
		defer f(&out)
	}
	if stop {
		return resp
	}

	if resp, stop = e.resolveStage(ctx, c); stop {
		return resp
	}

	resp, stop, fin = e.writeKeyStage(ctx, c)
	for _, f := range fin {
		defer f(&out)
	}
	if stop {
		return resp
	}

	resp, stop, fin = e.authorizeStage(ctx, c)
	for _, f := range fin {
		defer f(&out)
	}
	if stop {
		return resp
	}

	if resp, stop = e.policyStage(ctx, c); stop {
		return resp
	}

	resp, stop, fin = e.approvalStage(ctx, c)
	for _, f := range fin {
		defer f(&out)
	}
	if stop {
		return resp
	}

	if resp, stop = e.contributedStage(ctx, c); stop {
		return resp
	}

	result, resp, stop := e.dispatchStage(ctx, c)
	if stop {
		return resp
	}
	return e.resultStage(ctx, c, result)
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

// preview cuts b to at most n bytes, on a rune boundary. It used to cut at byte n
// whatever was there: a non-ASCII argument straddling the cut left invalid UTF-8
// in ArgsPreview, which protobuf refuses to marshal (the approval stream's Send
// failed and closed the stream) and Postgres refuses to store (the approval was
// not persisted, so the call was denied).
func preview(b []byte, n int) string {
	if len(b) <= n {
		return strings.ToValidUTF8(string(b), "")
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(b[cut]) {
		cut--
	}
	return strings.ToValidUTF8(string(b[:cut]), "")
}

// finishHistory closes a call's ActionHistory entry when Execute returns, from the
// call's final outcome (it runs deferred, after the handler's result is known): a
// call that was never dispatched is dropped (it touched nothing); a dispatched one
// that FAILED — the handler's error (an MCP isError result arrives as one), or a
// result that reports an error or a refusal in-band (actionStatus) — is dropped
// too: only a successful write counts as one a later call may duplicate (owner
// decision 2026-11-10, T-20261109-duplicate-notice-fires-on-the-retry-of-a-write).
// A successful one is done, with the identifiers its result names. A post_tool
// hook that withholds the output does not undo the call: it ran, so it is done.
func (e *ToolExecutor) finishHistory(ctx context.Context, ref ActionRef, dispatched bool, out ToolCallResponse) {
	if ref.IsZero() {
		return
	}
	if !dispatched || out.Error != "" {
		e.History.Drop(ref)
		return
	}
	result := out.ResultJSON
	if len(result) == 0 && out.ResultCID != "" && e.ContentStore != nil {
		if node, err := e.ContentStore.Get(context.WithoutCancel(ctx), CID(out.ResultCID)); err == nil && node != nil {
			result = node.Data
		}
	}
	if len(result) > 0 && actionStatus(result) != "ok" {
		e.History.Drop(ref)
		return
	}
	e.History.Finish(ref, PlanActionIDs(result))
}
