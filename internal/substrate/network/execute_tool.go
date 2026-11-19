package network

import (
	"context"
	"strconv"
	"strings"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"

	"encoding/json"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"time"
)

// agentIDFromMetadata reads the non-forgeable caller principal from x-agent-id
// gRPC metadata (the same mechanism QueryMemory uses). Empty when absent.
func agentIDFromMetadata(ctx context.Context) string {
	return mdValue(ctx, "x-agent-id")
}

// toolQueryFromMetadata reads the ADR-0044 relevance query (x-tool-query). Empty
// ⇒ the caller wants the full menu, not ranked retrieval.
func toolQueryFromMetadata(ctx context.Context) string {
	return mdValue(ctx, "x-tool-query")
}

// toolKFromMetadata reads the ADR-0044 menu size (x-tool-k); default 3.
func toolKFromMetadata(ctx context.Context) int {
	if v := mdValue(ctx, "x-tool-k"); v != "" {
		if k, err := strconv.Atoi(v); err == nil && k > 0 {
			return k
		}
	}
	return 3
}

// toolFullFromMetadata reads the ADR-0045 two-tier flag (x-tool-full). Absent or
// not "true" ⇒ Tier-1 (the terse menu); "true" ⇒ Tier-2 (full spec, the
// describe_tool path). Same metadata-transport convention as x-tool-query/k.
func toolFullFromMetadata(ctx context.Context) bool {
	return mdValue(ctx, "x-tool-full") == "true"
}

// toolNamesFromMetadata reads the ADR-0045 describe_tool target names
// (x-tool-names, comma-separated). Present ⇒ serve only those named (granted)
// tools; empty entries are dropped.
func toolNamesFromMetadata(ctx context.Context) []string {
	v := mdValue(ctx, "x-tool-names")
	if v == "" {
		return nil
	}
	var names []string
	for _, n := range strings.Split(v, ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	return names
}

func mdValue(ctx context.Context, key string) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get(key); len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

// ExecuteTool is the kernel-owned tool reference monitor RPC (ADR-0039 D4). The
// agent marshalled the args; the kernel authorizes (grant + resource policy +
// scope + approval) and runs the tool in a confined process. The principal is
// taken from x-agent-id metadata, never from the request body.
func (s *Server) ExecuteTool(ctx context.Context, req *pb.ExecuteToolRequest) (*pb.ExecuteToolResponse, error) {
	if s.ToolExecutor == nil {
		return nil, status.Error(codes.Unimplemented, "tool registry not configured")
	}
	// Resolve the caller's TASK SESSION from its lease and thread it into ctx. Tool calls
	// read content nodes (owner-gated by session) and write artifacts (stamped with a
	// session), and without this the executor saw no session at all: node reads fell back to
	// ownerless-only, and artifacts were stamped with the per-step lease instead — so
	// ListStepArtifacts(sessionID) could never find them.
	//
	// The lease is the one in the REQUEST (the SDK sends it there and in no header), made
	// the header every resolution below reads (withPresentedLease): before that, an SDK
	// tool call resolved no session, no conversation and no conversation owner, so a
	// dangerous-tool approval named no beneficiary
	// (T-20261107-tool-approvals-raised-by-chat-delegated-plan).
	sessionToken := leaseIDOf(req.GetLeaseId(), req.GetSessionTokenId())
	ctx = withPresentedLease(ctx, sessionToken)
	ctx = s.withCallerSession(ctx)

	// ADR-0098: tool calls are the other place a turn spends real time. The tool's NAME is
	// deliberately not reported — the closed phase vocabulary exists so a customer-facing
	// surface learns that work is happening, not what the deployment is made of.
	s.reportProgress(ctx, domain.PhaseRunningTool)

	agentID := agentIDFromMetadata(ctx)
	// Stamp the caller principal onto ctx for the write-side chokepoints. The authz
	// interceptor only seeds a principal for the premium agent-facing services, and
	// this RPC sits on /cambrian.Orchestrator/ — without the stamp the memory
	// classifier saw a zero principal on every tool-result write and the
	// mnemonic_action row landed untagged.
	if agentID != "" {
		ctx = domain.WithPrincipal(ctx, domain.AgentPrincipal(agentID))
	}
	// ADR-0139 D8: a lease not bound to this agent (another agent's, or an
	// agent-unbound one) confers nothing — not its skill
	// grants, not an evaluation lease's approval bypass, not its spend. Refused as a
	// denial (not an RPC error) so the agent's loop sees it like any other refusal.
	if s.leaseForeignToCaller(ctx, sessionToken) {
		return &pb.ExecuteToolResponse{Denied: true, DenyReason: "lease is not valid for this caller"}, nil
	}
	// ADR-0145 D12: the composed runtime acts only under a composed step lease.
	if why, refuse := s.composedToolRefusal(ctx, agentID); refuse {
		return &pb.ExecuteToolResponse{Denied: true, DenyReason: why}, nil
	}
	ctx = s.withRunPrincipal(ctx, sessionToken)
	// Work a chat turn ordered dies with the turn (turn_delegation.go): a lease of an
	// ended turn is refused BEFORE authorisation, so no approval can be raised for it,
	// and a call in flight when the turn ends is cancelled — which withdraws the
	// approval it may be waiting on (T-20261106-a-chat-turn-that-fails-deadline-exceeded-or).
	ctx, release, ok := s.admitTurnWork(ctx, sessionToken)
	if !ok {
		return &pb.ExecuteToolResponse{Denied: true, DenyReason: ErrTurnEnded.Error()}, nil
	}
	defer release()

	// The operator-facing counterpart, which DOES name the tool.
	//
	// Emitted around the call rather than after it: a page fetch or a web search
	// takes seconds, and a console that only learns about the call once it has
	// returned is not showing work in progress — which was the whole complaint
	// that this exists to answer.
	s.reportActivity(domain.AgentActivity{
		SessionTokenID: string(sessionToken), AgentID: agentID,
		Tool: req.GetToolName(), Args: redactedArgs(req.GetArgsJson()),
	})

	// ADR-0145 D1: where this call's ledger entry comes from (run, step, turn,
	// conversation, owner), resolved from the lease — also the scope ledger_find
	// searches within.
	if s.ledgerOn() {
		ctx = domain.WithLedgerProvenance(ctx, s.toolCallProvenance(ctx, sessionToken))
	}

	// ADR-0145 D9/D14: the plan step this call is for (run, step, the request
	// facts its guards need), from the lease binding, for the tool executor's
	// idempotency key and request-tied guards.
	if b, known := s.resolveLeaseFor(ctx, sessionToken); known && b.RunID != "" && b.StepIndex >= 0 {
		sc := domain.StepCall{RunID: b.RunID, StepIndex: b.StepIndex}
		if b.Guard != nil {
			sc.Guard = *b.Guard
		}
		ctx = domain.WithStepCall(ctx, sc)
	}

	// ADR-0145 D10: the execute_tool span's start event and start time (run_trace.go).
	traceID, toolStart := s.traceToolCallStart(ctx, agentID, sessionToken, req)
	var resp domain.ToolCallResponse
	if why, ok := s.unroutableBound(ctx, sessionToken, req.GetToolName()); !ok {
		// A running plan's earlier calls of this tool were all unroutable: refuse
		// without raising another approval nobody can answer (plan_actions.go).
		resp = domain.ToolCallResponse{Denied: true, DenyReason: why, ApprovalState: domain.ApprovalUnroutable}
	} else {
		resp = s.ToolExecutor.Execute(ctx, domain.ToolCallRequest{
			AgentID:  agentID,
			ToolName: req.GetToolName(),
			ArgsJSON: []byte(req.GetArgsJson()),
			// Phase 1: lease_id wins; the deprecated session_token_id is the fallback.
			SessionTokenID: sessionToken,
			TaskID:         mdValue(ctx, "x-task-id"), // ADR-0049 D3: per-step correlation key
		})
	}

	s.traceToolCall(ctx, traceID, toolStart, agentID, sessionToken, req, resp)
	// The plan's result reports every side-effecting call its steps made, so the
	// caller never re-orders a write that already happened (plan_actions.go).
	s.recordPlanAction(ctx, sessionToken, domain.ToolCallRequest{
		AgentID: agentID, ToolName: req.GetToolName(), ArgsJSON: []byte(req.GetArgsJson()),
	}, resp)
	s.reportActivity(domain.AgentActivity{
		SessionTokenID: string(sessionToken), AgentID: agentID,
		Tool: req.GetToolName(), Done: true,
		Denied: resp.Denied, Err: firstNonEmpty(resp.Error, resp.DenyReason),
	})
	return &pb.ExecuteToolResponse{
		ResultJson: wireResultJSON(resp),
		ResultCid:  resp.ResultCID,
		Denied:     resp.Denied,
		DenyReason: resp.DenyReason,
		Error:      wireError(resp),
		ArgHash:    resp.ArgHash,
		ResultHash: resp.ResultHash,
		// ADR-0139 D11 hook context, beside the result (contract 0104).
		Annotations: resp.Annotations,
	}, nil
}

// ListTools returns the system tools the calling agent may invoke, for building
// the agent's closed ReAct tool menu (ADR-0039). The principal is the
// x-agent-id metadata; the menu is advisory (ExecuteTool still authorizes every
// call). A missing executor yields an empty menu, never an error — the agent
// simply sees no system tools and reasons accordingly.
func (s *Server) ListTools(ctx context.Context, _ *pb.ListToolsRequest) (*pb.ListToolsResponse, error) {
	if s.ToolExecutor == nil {
		return &pb.ListToolsResponse{}, nil
	}
	// ADR-0126 phase 4 / ADR-0127 D4: resolve the caller's task session from its
	// lease and seed the task's beneficiary, so the menu built below can attach
	// the beneficiary's live worker fleet. This is the MENU half of the
	// contributed-tool chain; ExecuteTool re-derives the same facts at call time,
	// so a menu miss here can never widen what dispatch allows.
	ctx = s.withCallerSession(ctx)
	// ADR-0044: when the caller supplies a relevance query (via x-tool-query
	// metadata), serve the top-k task-relevant tools instead of the full menu.
	// k from x-tool-k (default 3). No query ⇒ the full granted menu (unchanged).
	agentID := agentIDFromMetadata(ctx)
	// ADR-0145 D12: the composed runtime without a composed lease has no menu.
	if _, _, runtimeOnly := s.composedFor(ctx, agentID); runtimeOnly {
		return &pb.ListToolsResponse{}, nil
	}
	var tools []domain.SystemTool
	switch {
	case len(toolNamesFromMetadata(ctx)) > 0:
		// ADR-0045 describe_tool: full spec for the named granted tools only
		// (grant-gated, fail-closed — ungranted names are simply absent).
		// ADR-0046 D6: tools a system skill conferred under the caller's OWN lease
		// are describable too, so a skill-granted tool reaches the model with its
		// real schema. A lease bound to another agent is ignored (no overlay).
		tools = s.ToolExecutor.AvailableToolsNamedFor(ctx, agentID, s.callerOverlayToken(ctx), toolNamesFromMetadata(ctx))
	case toolQueryFromMetadata(ctx) != "":
		tools = s.ToolExecutor.AvailableToolsRanked(ctx, agentID, toolQueryFromMetadata(ctx), toolKFromMetadata(ctx))
	default:
		tools = s.ToolExecutor.AvailableTools(ctx, agentID)
	}
	// ADR-0139 D12: a subagent sees only its toolset (the same lease ceiling
	// ExecuteTool enforces). A lease the caller may not use filters nothing here:
	// callerOverlayToken returns "" for it, and the menu stays the agent's own.
	if tok := s.callerOverlayToken(ctx); tok != "" {
		// ADR-0145 D12: a composed step's menu is exactly its composition (the
		// runtime holds no standing grants; the composed tools are its lease's
		// run-scoped grants, filtered by the same ceilings below).
		tools = s.withComposedTools(ctx, agentID, tok, tools)
		kept := tools[:0:0]
		for _, t := range tools {
			if s.ToolExecutor.WithinLeaseCeiling(tok, t.Name) {
				kept = append(kept, t)
			}
		}
		tools = kept
	}
	// ADR-0145: the kernel-served tools (ledger_get, ledger_find) are on every
	// menu — they need no grant and read only what the caller's predicate admits.
	// A describe_tool request lists them only when it names them.
	// ADR-0150: a kernel tool offered only to some leases (run_subtasks) is listed
	// by the caller's own lease.
	tools = appendKernelTools(tools, s.ToolExecutor.KernelToolSpecsFor(domain.LeaseID(s.callerOverlayToken(ctx))), toolNamesFromMetadata(ctx))
	// ADR-0045: serve Tier-1 (terse summary + arg-names-only schema) by default;
	// Tier-2 (full description + full schema) only when x-tool-full is set (the
	// describe_tool path). One render function (domain.ToolDisclosure) keeps the
	// served short form identical to the embedded one.
	s.reportToolsOutsideStep(ctx, agentID)
	full := toolFullFromMetadata(ctx)
	out := make([]*pb.ToolDescriptor, 0, len(tools))
	for _, t := range tools {
		desc, schemaJSON := domain.ToolDisclosure(t, full)
		out = append(out, &pb.ToolDescriptor{
			Name:        t.Name,
			Description: desc,
			SchemaJson:  schemaJSON,
			Dangerous:   t.Dangerous,
		})
	}
	return &pb.ListToolsResponse{Tools: out}, nil
}

// ToolOutsideStepHeader is the ListTools response header naming the tools that
// match a composed worker's find_tools query and exist within the person's access
// but are not in its step (comma-separated, ranked). They are NOT in the response
// body: the worker cannot load or call them. It reports them in its
// capability_missing outcome (domain.HandoffKeyCapabilityTool), so the re-planner
// can give a re-planned step a worker that has one
// (T-20261112-yield-sub-goal-on-a-composed-lease-runs-under, owner 2026-11-12).
// A header, like the request's x-tool-* parameters, so the proto is unchanged and
// a client that does not read it is unaffected.
const ToolOutsideStepHeader = "x-tool-outside-step"

// reportToolsOutsideStep sets ToolOutsideStepHeader on a find_tools query (a
// ranked ListTools with x-tool-query) made under a composed step lease. Ranked by
// the same retriever and k as the menu; nothing when no retriever, no match over
// its relevance floor, or the caller is not on a composed lease.
func (s *Server) reportToolsOutsideStep(ctx context.Context, agentID string) {
	query := toolQueryFromMetadata(ctx)
	if s.ToolsOutsideStep == nil || query == "" || len(toolNamesFromMetadata(ctx)) > 0 ||
		s.ToolExecutor == nil || s.ToolExecutor.Retriever == nil {
		return
	}
	comp, composed, _ := s.composedFor(ctx, agentID)
	if !composed {
		return
	}
	outside := s.ToolsOutsideStep(ctx, comp)
	if len(outside) == 0 {
		return
	}
	names := make([]string, len(outside))
	for i, t := range outside {
		names[i] = t.Name
	}
	ranked, err := s.ToolExecutor.Retriever.Rank(ctx, query, names, toolKFromMetadata(ctx))
	if err != nil || len(ranked) == 0 {
		return
	}
	if err := grpc.SetHeader(ctx, metadata.Pairs(ToolOutsideStepHeader, strings.Join(ranked, ","))); err != nil {
		slog.Debug("ListTools: outside-step header not set", "err", err)
	}
}

// WatchApprovals streams pending dangerous-tool approval requests to a subscribed
// operator (ADR-0039 D10). Operator-plane only: the operator auth interceptor
// requires the operator bearer for this method (authz.IsOperatorGatedAgentServiceMethod,
// operator.authorize), so an agent — or any client without an operator token —
// is refused before it gets here. The x-agent-id check below is defence in depth.
func (s *Server) WatchApprovals(_ *pb.WatchApprovalsRequest, stream pb.Orchestrator_WatchApprovalsServer) error {
	if s.ApprovalHub == nil {
		return status.Error(codes.Unimplemented, "approval channel not configured")
	}
	op, err := approvalOperator(stream.Context())
	if err != nil {
		return err
	}
	// ADR-0142 D11: addressed — the items this operator may answer, pending only
	// (this stream predates resolution events; they ride the operator feed).
	ch, cancel := s.ApprovalHub.Watch(domain.ApprovalWatch{For: approverOf(stream.Context(), op)})
	defer cancel()
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			if ev.Type != domain.ApprovalEventRaised {
				continue
			}
			r := ev.Item
			ben := ""
			if !r.Beneficiary.IsZero() {
				ben = r.Beneficiary.String()
			}
			if err := stream.Send(&pb.ApprovalRequest{
				Id: r.ID, AgentId: r.AgentID, ToolName: r.ToolName, ArgsPreview: r.ArgsPreview, Kind: r.Kind,
				Rule: r.RuleSummary(), Beneficiary: ben, ActionClass: r.ActionClass,
				DuplicateNotice: r.DuplicateNotice,
			}); err != nil {
				return err
			}
		}
	}
}

// admitTurnWork is the agent plane's one admission for a call made under a lease
// that may belong to a chat turn: ExecuteTool, and every other RPC that can block
// on a human approval (ProposeScheduledTask, a skill access request on a full
// skill load). Before ENG-2119 only ExecuteTool had it, so a schedule proposal's
// 300 s approval wait read as silence to the chat stall watchdog, and its RPC —
// running on the SDK worker's context, which the turn's cancel never reaches —
// kept the approval pending after the turn had failed: the late approve created
// the task (T-20261118-a-chat-turn-waiting-on-a-human-approval-is).
//
// A lease of an ended turn is refused (ok=false) before anything runs — no
// approval can be raised for it. Otherwise the call runs under a context the
// turn's end cancels, which withdraws the approval it may wait on (a late answer
// is then refused with the reason, domain.AddressedHub.Answer); its approval
// waits count as the turn waiting on a person (TurnDelegations.PersonWait); and
// it is work in flight on its lease for as long as it runs, so neither idle
// watchdog reads a page fetch, a confined process or an approval wait as silence
// and the lease is not swept under it
// (T-20261105-owner-decision-needed-120-s-plan-timeout-ms). release must run when
// the call returns. Work no chat turn ordered passes the turn gate untouched.
func (s *Server) admitTurnWork(ctx context.Context, lease domain.LeaseID) (context.Context, func(), bool) {
	releaseGuard := func() {}
	if s.Delegations != nil {
		b, known := s.resolveLeaseFor(ctx, lease)
		gctx, rel, ok := s.Delegations.GuardToolCall(ctx, lease, b, known)
		if !ok {
			return ctx, func() {}, false
		}
		ctx, releaseGuard = gctx, rel
	}
	untrack := s.trackLeaseWork(lease)
	return ctx, func() { untrack(); releaseGuard() }, true
}

// SubmitApprovalDecision resolves a pending approval (ADR-0039 D10). Operator-plane
// only, behind the operator bearer like WatchApprovals.
//
// The approver recorded is the AUTHENTICATED operator, never the request's
// approver_id (INV-5: identity is established by the kernel, not read from a
// payload). The field stays in the proto for wire compatibility and is ignored;
// honouring it would let one operator attribute an approval to another.
func (s *Server) SubmitApprovalDecision(ctx context.Context, req *pb.ApprovalDecisionRequest) (*pb.ApprovalDecisionResponse, error) {
	if s.ApprovalHub == nil {
		return nil, status.Error(codes.Unimplemented, "approval channel not configured")
	}
	approver, err := approvalOperator(ctx)
	if err != nil {
		return nil, err
	}
	// ADR-0142 D11: the hub authorises the answer — an operator who satisfies
	// none of the item's rules (a self-ruled machine consent) is refused (ok=false).
	_, aerr := s.ApprovalHub.Answer(ctx, req.GetId(), domain.ApprovalAnswer{
		Approved: req.GetApprove(), By: approverOf(ctx, approver),
	})
	return &pb.ApprovalDecisionResponse{Ok: aerr == nil}, nil
}

// approverOf is the verified answerer on the operator-gated agent-service
// approval methods: the interceptor admitted only the operator role to them
// (operator.isOperatorOnly), so the caller holds "operator" plus whatever full
// role set the interceptor stamped.
func approverOf(ctx context.Context, p domain.PrincipalRef) domain.Approver {
	roles := append([]string(nil), domain.PrincipalRolesFromContext(ctx)...)
	has := false
	for _, r := range roles {
		if r == domain.ApprovalRoleOperator {
			has = true
		}
	}
	if !has {
		roles = append(roles, domain.ApprovalRoleOperator)
	}
	return domain.Approver{Principal: p, Roles: roles}
}

// approvalOperator returns the operator principal the auth interceptor stamped.
//
// It does not authenticate anything — token validation is operator.authorize's
// job alone. It refuses when that stamp is missing (the interceptor did not run,
// e.g. a server assembled without it), so the approval plane fails CLOSED rather
// than reverting to the old "no x-agent-id means operator" rule. A caller that
// also presents x-agent-id is refused: an agent never approves tools.
func approvalOperator(ctx context.Context) (domain.PrincipalRef, error) {
	if agentIDFromMetadata(ctx) != "" {
		return domain.PrincipalRef{}, status.Error(codes.PermissionDenied, "approval plane is operator-only")
	}
	p := domain.PrincipalFromContext(ctx)
	if p.IsZero() || p.Kind != domain.PrincipalUser {
		return domain.PrincipalRef{}, status.Error(codes.Unauthenticated, "approval plane requires an authenticated operator")
	}
	return p, nil
}

// reportActivity fans one activity record out, best-effort.
//
// Silent when nobody is listening or when the caller had no managed session:
// with no session token there is no one to correlate the record to, and an
// unattributable activity record is noise rather than observability.
func (s *Server) reportActivity(a domain.AgentActivity) {
	if s.Activity == nil || a.SessionTokenID == "" {
		return
	}
	if a.At.IsZero() {
		a.At = time.Now()
	}
	s.Activity.ObserveAgentActivity(a)
}

// redactedArgs parses the agent's arg blob for display.
//
// Unparseable args are reported as absent rather than raw: the blob is whatever
// a model produced, and pasting it into an operator console unparsed is how a
// prompt-injected string gets rendered somewhere it was never meant to reach.
func redactedArgs(argsJSON string) map[string]string {
	if strings.TrimSpace(argsJSON) == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &m); err != nil {
		return nil
	}
	return domain.RedactArgs(m)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// withRunPrincipal stamps the person this call is FOR on ctx (ADR-0140 D4),
// resolved from kernel-held facts on the presented lease under the ADR-0139 D8
// ownership rule — never from the request, the model or a header. A per_user
// integration selects that person's credential by it; with no lease, no
// binding or no resolver the context carries none, and such a tool fails
// closed ("not acting for a person"), never with someone else's credential.
func (s *Server) withRunPrincipal(ctx context.Context, lease domain.LeaseID) context.Context {
	if s.RunPrincipal == nil || lease == "" {
		return ctx
	}
	b, ok := s.resolveLeaseFor(ctx, lease)
	if !ok {
		return ctx
	}
	if p := s.RunPrincipal(ctx, b); p != "" {
		return domain.WithRunPrincipal(ctx, p)
	}
	return ctx
}

// appendKernelTools adds the kernel tools to a menu: all of them, or only those
// named when the menu is a describe_tool request. A name already on the menu is
// not repeated.
func appendKernelTools(tools, kernel []domain.SystemTool, named []string) []domain.SystemTool {
	if len(kernel) == 0 {
		return tools
	}
	have := make(map[string]bool, len(tools))
	for _, t := range tools {
		have[t.Name] = true
	}
	want := map[string]bool{}
	for _, n := range named {
		want[n] = true
	}
	for _, k := range kernel {
		if have[k.Name] || (len(named) > 0 && !want[k.Name]) {
			continue
		}
		tools = append(tools, k)
	}
	return tools
}

// wireResultJSON is result_json as the agent receives it: the result, or — for a
// result that travels as its reference — its summary ({"_offloaded": {ref, size,
// content type, shape, preview, pages, read}}) beside result_cid, so the model is
// never handed a bare id it can only re-open whole (owner decision 2026-11-12,
// T-20261112-worker-resolve-cid-inline-has-no-size-cap-a). No proto change: an SDK
// that predates the summary reads both fields as "an inline result plus its
// reference", which still shows the preview and the read call.
//
// An untrusted (MCP) response is fenced as data here — the agent-facing
// boundary — by domain.AgentToolResult (ADR-0043 D4, ENG-117); wireError is its
// error half.
func wireResultJSON(resp domain.ToolCallResponse) string {
	result, _ := domain.AgentToolResult(resp)
	return result
}

// wireError is the error the agent receives: a tool-reported error of an
// untrusted response fenced as data, any other error as it is.
func wireError(resp domain.ToolCallResponse) string {
	_, errText := domain.AgentToolResult(resp)
	return errText
}
