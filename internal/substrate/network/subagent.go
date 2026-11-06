package network

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
)

// ADR-0139 D12: in-agent subagents (Orchestrator.SpawnSubagent, C7).
//
// A parent agent asks the kernel to run a CHILD: a separate kernel dispatch on
// its own lease. Everything that makes that safe is decided here, server-side,
// from the parent's lease — never from the request or the model:
//
//   - identity: the presented lease must be live and bound to the caller (the
//     ADR-0139 D8 rule, resolveLeaseFor); a missing, foreign or expired lease is
//     PERMISSION_DENIED;
//   - depth: the number of ParentLeaseID hops from the parent's lease to its root
//     (domain.LeaseBinding.ParentLeaseID, written only here). A request cannot
//     state it;
//   - toolset: resolved against the tools the PARENT may call (its menu, its
//     run-scoped skill grants, and its own lease ceiling when it is itself a
//     child). Any entry reaching outside that set refuses the spawn (DENIED); it
//     is never silently narrowed. The resolved names become the child lease's
//     ceiling (domain.ToolExecutor.LeaseCeiling), so the ToolExecutor enforces
//     them on every child call, under tools_unrestricted and over skill grants;
//   - spend: the child lease is bound to the parent's session (the ADR-0131
//     session ceiling binds every child call), its usage rolls up to the parent
//     lease on completion (and so up the chain to the root), and the lease gate
//     enforces the child's token_budget and the tree-wide token cap per call;
//   - limits: depth, per-root concurrency, the tree-wide tool-round cap, default
//     rounds/timeout and the result cap are execution.agents.subagent_*.
//
// The child runs through s.Dispatcher — CallAgent for the parent's own profile
// or a named agent, Execute (selection) for a capability — exactly as a plan
// step does. The only difference is domain.WithDedicatedInstance: agents are
// single-threaded (SDK runtime MAX_WORKERS=1), so a child on its parent's busy
// instance would wait for its own parent. A dedicated instance is booted for the
// call and evicted after it, which is also how a cancelled spawn stops the child.

// ErrSubagentBudget is the lease gate's refusal of a model call on a child lease
// whose token_budget, or whose tree's token cap, is spent.
var ErrSubagentBudget = errors.New("subagent budget exhausted")

// Handoff context keys the child dispatch carries (kernel → SDK). The SDK reads
// them to shape the child's run; the kernel enforces every limit itself.
const (
	SubagentKeyMarker         = "_subagent"
	SubagentKeyMode           = "_subagent_mode"
	SubagentKeyDepth          = "_subagent_depth"
	SubagentKeyMayspawn       = "_subagent_may_spawn"
	SubagentKeyMaxToolRounds  = "_subagent_max_tool_rounds"
	SubagentKeyToolset        = "_subagent_toolset"
	SubagentKeyOutputSchema   = "_subagent_output_schema"
	SubagentKeyContextSummary = "_subagent_context_summary"
	SubagentKeyContextRefs    = "_subagent_context_refs"
	SubagentKeyRunID          = "_subagent_run_id"
)

// maxLeaseChain bounds the parent walk: far beyond any configured depth, so it
// only ever stops a corrupt (cyclic) chain.
const maxLeaseChain = 64

// SubagentService holds the D12 runtime state: one tree per ROOT lease
// (concurrency slots, tree-wide accumulators, idempotency) and one entry per
// live child lease (its ceiling and counters). It implements
// domain.LeaseToolCeiling (the ToolExecutor's per-lease ceiling) and LeaseGate
// (the gateway's per-call admission). No goroutines: stale trees are swept on
// the next spawn.
type SubagentService struct {
	cfg   config.AgentsConfig
	hooks *domain.HookChain

	mu       sync.Mutex
	trees    map[domain.LeaseID]*subagentTree
	children map[domain.LeaseID]*subagentChild
}

type subagentTree struct {
	root       domain.LeaseID
	slots      chan struct{}
	tokens     int64
	toolRounds int
	active     int
	spawns     map[string]*spawnEntry
}

type spawnEntry struct {
	done chan struct{}
	resp *pb.SpawnSubagentResponse
}

type subagentChild struct {
	tree          *subagentTree
	parent        domain.LeaseID
	tools         map[string]bool
	maxToolRounds int
	toolRounds    int
	tokenBudget   int64
	tokens        int64
	budgetHit     bool
	denials       int
	artifacts     []string
}

// NewSubagentService builds the service over the Agents config block and the
// D11 hook chain (subagent_start / subagent_end; nil = no hooks).
func NewSubagentService(cfg config.AgentsConfig, hooks *domain.HookChain) *SubagentService {
	return &SubagentService{
		cfg:      cfg,
		hooks:    hooks,
		trees:    map[domain.LeaseID]*subagentTree{},
		children: map[domain.LeaseID]*subagentChild{},
	}
}

var (
	_ domain.LeaseToolCeiling = (*SubagentService)(nil)
	_ LeaseGate               = (*SubagentService)(nil)
)

// AllowsLease implements domain.LeaseToolCeiling.
func (s *SubagentService) AllowsLease(lease, tool string) (inside, bound bool) {
	if s == nil {
		return false, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.children[domain.LeaseID(lease)]
	if !ok {
		return false, false
	}
	return c.tools[tool], true
}

// AdmitLeaseCall implements LeaseGate: a child lease past its own token_budget
// or its tree's token cap gets no further model call.
func (s *SubagentService) AdmitLeaseCall(lease domain.LeaseID) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.children[lease]
	if !ok {
		return nil
	}
	if c.tokenBudget > 0 && c.tokens >= c.tokenBudget {
		c.budgetHit = true
		return fmt.Errorf("%w: the child's token budget (%d) is spent", ErrSubagentBudget, c.tokenBudget)
	}
	if limit := int64(s.cfg.SubagentTreeTokenBudget); limit > 0 && c.tree.tokens >= limit {
		c.budgetHit = true
		return fmt.Errorf("%w: the tree-wide token budget (%d) is spent", ErrSubagentBudget, limit)
	}
	return nil
}

// ObserveLeaseUsage implements LeaseGate: every booked call on a child lease
// counts toward the child and its tree.
func (s *SubagentService) ObserveLeaseUsage(lease domain.LeaseID, u llm.TokenUsage) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.children[lease]; ok {
		c.tokens += int64(u.TotalTokens)
		c.tree.tokens += int64(u.TotalTokens)
	}
}

// AdmitToolCall counts one tool call on a child lease against the child's
// max_tool_rounds and the tree-wide iteration cap. A refusal is reported to the
// agent as a denial. Non-child leases are always admitted.
func (s *SubagentService) AdmitToolCall(lease domain.LeaseID) (bool, string) {
	if s == nil || lease == "" {
		return true, ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.children[lease]
	if !ok {
		return true, ""
	}
	if c.toolRounds >= c.maxToolRounds {
		c.budgetHit = true
		return false, fmt.Sprintf("subagent tool-round cap reached (%d); finish with final_answer", c.maxToolRounds)
	}
	if limit := s.cfg.SubagentTreeMaxToolRounds; limit > 0 && c.tree.toolRounds >= limit {
		c.budgetHit = true
		return false, fmt.Sprintf("tree-wide subagent tool-round cap reached (%d); finish with final_answer", limit)
	}
	c.toolRounds++
	c.tree.toolRounds++
	return true, ""
}

// RecordToolResult notes a child tool call's outcome: a denial (for the DENIED
// exit reason) and an offloaded result CID (for the result's artifacts).
func (s *SubagentService) RecordToolResult(lease domain.LeaseID, resultCID string, denied bool) {
	if s == nil || lease == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.children[lease]
	if !ok {
		return
	}
	if denied {
		c.denials++
	}
	if resultCID != "" {
		c.artifacts = append(c.artifacts, resultCID)
	}
}

// treeFor returns (creating) the tree of root, and sweeps trees whose root lease
// is gone and that run no child. Callers hold s.mu.
func (s *SubagentService) treeForLocked(root domain.LeaseID, live func(domain.LeaseID) bool) *subagentTree {
	for id, t := range s.trees {
		if id != root && t.active == 0 && !live(id) {
			delete(s.trees, id)
		}
	}
	t, ok := s.trees[root]
	if !ok {
		n := s.cfg.SubagentMaxConcurrency
		if n <= 0 {
			n = 1
		}
		t = &subagentTree{root: root, slots: make(chan struct{}, n), spawns: map[string]*spawnEntry{}}
		s.trees[root] = t
	}
	return t
}

// subagentGateway is the gateway surface a spawn needs beyond LLMGateway.
type subagentGateway interface {
	RollUpLease(parent domain.LeaseID, u llm.TokenUsage)
}

// SpawnSubagent implements the ADR-0139 D12 RPC. See the file comment.
func (s *Server) SpawnSubagent(ctx context.Context, req *pb.SpawnSubagentRequest) (*pb.SpawnSubagentResponse, error) {
	if s.Subagents == nil || s.Dispatcher == nil || s.LLMGateway == nil || s.leaseResolver() == nil {
		return nil, status.Error(codes.Unimplemented, "subagents are not enabled on this kernel")
	}
	if err := validateSpawnShape(req); err != nil {
		return nil, err
	}
	lease := domain.LeaseID(req.GetLeaseId())
	if lease == "" {
		lease = domain.LeaseID(mdValue(ctx, leaseHeader))
	}
	if lease == "" {
		return nil, status.Error(codes.PermissionDenied, "a lease is required")
	}
	parent, ok := s.resolveLeaseFor(ctx, lease)
	if !ok || parent.AgentID == "" {
		return nil, status.Error(codes.PermissionDenied, "lease is missing, foreign or expired")
	}
	ctx = s.withCallerSession(ctx)
	if p := domain.PrincipalFromContext(ctx); p.IsZero() {
		ctx = domain.WithPrincipal(ctx, domain.AgentPrincipal(parent.AgentID))
	}
	return s.spawn(ctx, lease, parent, req)
}

// validateSpawnShape is the INVALID_ARGUMENT half: request-shape faults only.
func validateSpawnShape(req *pb.SpawnSubagentRequest) error {
	if strings.TrimSpace(req.GetObjective()) == "" {
		return status.Error(codes.InvalidArgument, "objective is required")
	}
	if req.GetTargetAgentId() != "" && req.GetTargetCapability() != "" {
		return status.Error(codes.InvalidArgument, "set target_agent_id or target_capability, not both")
	}
	switch req.GetMode() {
	case pb.SubagentMode_SUBAGENT_MODE_UNSPECIFIED, pb.SubagentMode_SUBAGENT_MODE_ISOLATED:
		if req.GetContextSummary() != "" || len(req.GetContextRefs()) > 0 {
			return status.Error(codes.InvalidArgument, "context_summary and context_refs are FORK-only")
		}
	case pb.SubagentMode_SUBAGENT_MODE_FORK:
	default:
		return status.Error(codes.InvalidArgument, "unknown mode")
	}
	if req.GetMaxToolRounds() < 0 || req.GetTimeoutMs() < 0 || req.GetTokenBudget() < 0 {
		return status.Error(codes.InvalidArgument, "max_tool_rounds, timeout_ms and token_budget must be >= 0")
	}
	if sch := req.GetOutputSchema(); sch != nil && sch.GetSchemaJson() != "" {
		if err := llm.CompileResponseSchema([]byte(sch.GetSchemaJson())); err != nil {
			return status.Errorf(codes.InvalidArgument, "output_schema: %v", err)
		}
	}
	for _, e := range req.GetToolset() {
		if strings.ContainsAny(e, "[]") {
			return status.Errorf(codes.InvalidArgument, "toolset entry %q: only * and ? globs are supported", e)
		}
	}
	return nil
}

// leaseChain walks ParentLeaseID from lease to the root. depth is the number of
// hops (0 for a root lease). ok=false for a broken or cyclic chain.
func leaseChain(r domain.LeaseResolver, lease domain.LeaseID, b domain.LeaseBinding) (root domain.LeaseID, depth int, ok bool) {
	root, cur := lease, b
	for cur.ParentLeaseID != "" {
		depth++
		if depth > maxLeaseChain {
			return "", 0, false
		}
		next, known := r.ResolveLease(cur.ParentLeaseID)
		if !known {
			return "", 0, false
		}
		root, cur = cur.ParentLeaseID, next
	}
	return root, depth, true
}

func spawnResult(st pb.SubagentStatus, reason pb.SubagentExitReason, detail string) *pb.SpawnSubagentResponse {
	return &pb.SpawnSubagentResponse{Status: st, ExitReason: reason, Detail: detail}
}

func failed(reason pb.SubagentExitReason, detail string) *pb.SpawnSubagentResponse {
	return spawnResult(pb.SubagentStatus_SUBAGENT_STATUS_FAILED, reason, detail)
}

// childRunID names the child dispatch: derived from (root, client_spawn_id)
// when the spawn is keyed, so a retry names the same run; random otherwise.
func childRunID(root domain.LeaseID, key string) string {
	if key != "" {
		sum := sha256.Sum256([]byte(string(root) + "|" + key))
		return "subagent-" + hex.EncodeToString(sum[:8])
	}
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand never fails on supported platforms
	return "subagent-" + hex.EncodeToString(b[:])
}

func (s *Server) spawn(ctx context.Context, lease domain.LeaseID, parent domain.LeaseBinding, req *pb.SpawnSubagentRequest) (*pb.SpawnSubagentResponse, error) {
	svc := s.Subagents
	resolver := s.leaseResolver()
	root, depth, ok := leaseChain(resolver, lease, parent)
	if !ok {
		return nil, status.Error(codes.PermissionDenied, "lease chain is broken")
	}
	live := func(id domain.LeaseID) bool { _, known := resolver.ResolveLease(id); return known }

	// Idempotency (AGENTS.md 2.6): one result per (root, client_spawn_id).
	key := req.GetClientSpawnId()
	svc.mu.Lock()
	tree := svc.treeForLocked(root, live)
	var entry *spawnEntry
	if key != "" {
		if prev, seen := tree.spawns[key]; seen {
			svc.mu.Unlock()
			select {
			case <-prev.done:
				if prev.resp == nil { // the first attempt was cancelled; nothing to replay
					return nil, status.Error(codes.Aborted, "the first spawn with this client_spawn_id was cancelled")
				}
				return proto.Clone(prev.resp).(*pb.SpawnSubagentResponse), nil
			case <-ctx.Done():
				return s.endOfCall(ctx, failedTimeout("timed out waiting for the first spawn with this client_spawn_id"))
			}
		}
		entry = &spawnEntry{done: make(chan struct{})}
		tree.spawns[key] = entry
	}
	tree.active++
	svc.mu.Unlock()
	defer func() {
		svc.mu.Lock()
		tree.active--
		svc.mu.Unlock()
	}()

	resp, err := s.runSpawn(ctx, svc, tree, lease, parent, depth, req, childRunID(root, key))
	if entry != nil {
		svc.mu.Lock()
		if err == nil && resp != nil {
			entry.resp = proto.Clone(resp).(*pb.SpawnSubagentResponse)
		} else {
			delete(tree.spawns, key) // a cancelled first try may be retried afresh
		}
		svc.mu.Unlock()
		close(entry.done)
	}
	return resp, err
}

func failedTimeout(detail string) *pb.SpawnSubagentResponse {
	return spawnResult(pb.SubagentStatus_SUBAGENT_STATUS_STOPPED, pb.SubagentExitReason_SUBAGENT_EXIT_REASON_TIMEOUT, detail)
}

// endOfCall turns a result into the RPC answer: when the CALLER cancelled, the
// call is cancelled (nobody is listening for an OK body).
func (s *Server) endOfCall(ctx context.Context, resp *pb.SpawnSubagentResponse) (*pb.SpawnSubagentResponse, error) {
	if errors.Is(ctx.Err(), context.Canceled) {
		return nil, status.Error(codes.Canceled, "spawn cancelled by the caller")
	}
	return resp, nil
}

// runSpawn is one spawn after identity and idempotency.
func (s *Server) runSpawn(ctx context.Context, svc *SubagentService, tree *subagentTree, lease domain.LeaseID,
	parent domain.LeaseBinding, depth int, req *pb.SpawnSubagentRequest, runID string) (*pb.SpawnSubagentResponse, error) {
	cfg := svc.cfg
	childDepth := depth + 1
	if cfg.SubagentMaxDepth <= 0 || childDepth > cfg.SubagentMaxDepth {
		r := failed(pb.SubagentExitReason_SUBAGENT_EXIT_REASON_DEPTH,
			fmt.Sprintf("spawn depth %d exceeds execution.agents.subagent_max_depth (%d)", childDepth, cfg.SubagentMaxDepth))
		r.ChildRunId = runID
		return r, nil
	}

	tools, overlay, memWrite, why := s.resolveSubagentToolset(ctx, svc, lease, parent, req.GetToolset())
	if why != "" {
		r := failed(pb.SubagentExitReason_SUBAGENT_EXIT_REASON_DENIED, why)
		r.ChildRunId = runID
		return r, nil
	}

	childAgent := parent.AgentID
	switch {
	case req.GetTargetAgentId() != "":
		childAgent = req.GetTargetAgentId()
		if why := s.subagentTargetDenied(ctx, parent, childAgent); why != "" {
			r := failed(pb.SubagentExitReason_SUBAGENT_EXIT_REASON_DENIED, why)
			r.ChildRunId = runID
			return r, nil
		}
	case req.GetTargetCapability() != "":
		childAgent = "" // chosen by selection below
	}

	names := make([]string, 0, len(tools))
	for n := range tools {
		names = append(names, n)
	}
	sort.Strings(names)
	detail := map[string]string{
		"child_run_id":      runID,
		"mode":              req.GetMode().String(),
		"objective":         truncateRunes(req.GetObjective(), 500),
		"target_agent_id":   req.GetTargetAgentId(),
		"target_capability": req.GetTargetCapability(),
		"toolset":           strings.Join(names, ","),
		"depth":             strconv.Itoa(childDepth),
	}
	hookEv := domain.HookEvent{
		RunID:      string(parent.SessionID),
		LeaseID:    string(lease),
		AgentID:    parent.AgentID,
		Principals: domain.HookPrincipalChain(ctx, parent.AgentID),
		Verdict:    domain.HookAllow,
	}
	start := hookEv
	start.Point = domain.HookSubagentStart
	start.Detail = detail
	if out := svc.hooks.Run(ctx, start); out.Verdict != domain.HookAllow {
		r := failed(pb.SubagentExitReason_SUBAGENT_EXIT_REASON_DENIED, out.DenyReason())
		r.ChildRunId = runID
		return r, nil
	}
	resp, err := s.runChild(ctx, svc, tree, lease, parent, childDepth, childAgent, tools, overlay, memWrite, req, runID)
	if err != nil {
		return nil, err
	}
	end := hookEv
	end.Point = domain.HookSubagentEnd
	end.Detail = map[string]string{
		"child_run_id": runID,
		"status":       resp.GetStatus().String(),
		"exit_reason":  resp.GetExitReason().String(),
		"tool_rounds":  strconv.Itoa(int(resp.GetToolRounds())),
		"truncated":    strconv.FormatBool(resp.GetTruncated()),
	}
	svc.hooks.Run(ctx, end) // observe-only: a finished child cannot be un-run
	return resp, nil
}

// resolveSubagentToolset resolves the requested toolset against the tools the
// parent may call. It returns the child's tool names, the subset that reached
// the parent through its run-scoped skill overlay (re-conferred on the child
// lease), whether memory write was granted, and a non-empty reason when the
// spawn must be refused.
func (s *Server) resolveSubagentToolset(ctx context.Context, svc *SubagentService, lease domain.LeaseID,
	parent domain.LeaseBinding, entries []string) (tools map[string]bool, overlay []string, memWrite bool, why string) {
	cfg := svc.cfg
	parentSet := map[string]bool{}
	universe := map[string]bool{}
	te := s.ToolExecutor
	if te != nil {
		var names []string
		if te.Registry != nil {
			for _, t := range te.Registry.All() {
				names = append(names, t.Name)
				universe[t.Name] = true
			}
		}
		for _, t := range te.AvailableTools(ctx, parent.AgentID) {
			if !universe[t.Name] {
				names = append(names, t.Name)
				universe[t.Name] = true
			}
		}
		for _, t := range te.AvailableToolsNamedFor(ctx, parent.AgentID, string(lease), names) {
			parentSet[t.Name] = true
		}
	}
	tools = map[string]bool{}
	for _, raw := range entries {
		e := strings.TrimSpace(raw)
		if e == "" {
			continue
		}
		if domain.ToolAllowlist(cfg.SubagentMemoryWriteTools).Allows(e) {
			// A control verb (D14 ProposeMemoryNote), not a registry tool: the
			// parent can hand on only a right it holds itself.
			if parent.IsChild() && !parent.MemoryWriteGranted {
				return nil, nil, false, fmt.Sprintf("toolset entry %q: memory write is not the parent's to grant", e)
			}
			memWrite = true
			if parentSet[e] {
				tools[e] = true
			}
			continue
		}
		if e == "*" {
			for n := range parentSet {
				if !domain.ToolAllowlist(cfg.SubagentWithheldTools).Allows(n) {
					tools[n] = true
				}
			}
			continue
		}
		matched := false
		for n := range universe {
			if !domain.MatchToolGlob(e, n) {
				continue
			}
			if !parentSet[n] {
				return nil, nil, false, fmt.Sprintf("toolset entry %q matches %q, which the parent may not call", e, n)
			}
			tools[n] = true
			matched = true
		}
		if !matched {
			return nil, nil, false, fmt.Sprintf("toolset entry %q matches no tool the parent may call", e)
		}
	}
	if te != nil {
		for n := range tools {
			if te.Overlay.Granted(string(lease), n) {
				overlay = append(overlay, n)
			}
		}
	}
	return tools, overlay, memWrite, ""
}

// subagentTargetDenied checks a named target: it must exist and the decision
// point must let the parent dispatch to it (KindAgent). "" = allowed.
func (s *Server) subagentTargetDenied(ctx context.Context, parent domain.LeaseBinding, target string) string {
	if s.Manager != nil && s.Manager.Registry != nil {
		if def, err := s.Manager.Registry.GetAgentByName(ctx, target); err != nil || def == nil {
			return fmt.Sprintf("target %q is not an agent the principal may dispatch to", target)
		}
	}
	if s.Authz != nil {
		dec := s.Authz.Authorize(ctx, domain.AccessRequest{
			Principal: domain.AgentPrincipal(parent.AgentID),
			Surface:   domain.SurfaceFromContext(ctx),
			Resource:  domain.ResourceRef{Kind: domain.KindAgent, ID: target},
			Session:   parent.SessionID,
		})
		if !dec.Allowed {
			return fmt.Sprintf("target %q is not an agent the principal may dispatch to: %s", target, dec.Detail)
		}
	}
	return ""
}

// childDeadline is the child's wall-clock end: timeout_ms (or the default),
// clamped to just before the parent call's own deadline so the TIMEOUT result
// can still reach the parent.
func childDeadline(ctx context.Context, cfg config.AgentsConfig, timeoutMs int64) time.Time {
	if timeoutMs <= 0 {
		timeoutMs = int64(cfg.SubagentDefaultTimeoutMs)
	}
	if timeoutMs <= 0 {
		timeoutMs = 300000
	}
	dl := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	if pdl, ok := ctx.Deadline(); ok {
		margin := time.Until(pdl) / 10
		if margin > time.Second {
			margin = time.Second
		}
		if p := pdl.Add(-margin); p.Before(dl) {
			dl = p
		}
	}
	return dl
}

func (s *Server) runChild(ctx context.Context, svc *SubagentService, tree *subagentTree, lease domain.LeaseID,
	parent domain.LeaseBinding, depth int, childAgent string, tools map[string]bool, overlay []string,
	memWrite bool, req *pb.SpawnSubagentRequest, runID string) (*pb.SpawnSubagentResponse, error) {
	cfg := svc.cfg
	withRun := func(r *pb.SpawnSubagentResponse) *pb.SpawnSubagentResponse { r.ChildRunId = runID; return r }

	// The session ceiling (ADR-0131 D3) binds a child like any step.
	if err := s.SpendMeter.AdmitSession(parent.SessionID, string(domain.PurposeAgentStep), 0); err != nil {
		return withRun(spawnResult(pb.SubagentStatus_SUBAGENT_STATUS_STOPPED, pb.SubagentExitReason_SUBAGENT_EXIT_REASON_BUDGET, err.Error())), nil
	}

	// Narrow-only limits, clamped to what the tree has left.
	rounds := int(req.GetMaxToolRounds())
	if rounds <= 0 {
		rounds = cfg.SubagentDefaultMaxToolRounds
	}
	budget := req.GetTokenBudget()
	svc.mu.Lock()
	if limit := cfg.SubagentTreeMaxToolRounds; limit > 0 {
		if left := limit - tree.toolRounds; left < rounds {
			rounds = left
		}
	}
	treeSpent := false
	if limit := int64(cfg.SubagentTreeTokenBudget); limit > 0 {
		left := limit - tree.tokens
		treeSpent = left <= 0
		if budget == 0 || budget > left {
			budget = left
		}
	}
	if rounds <= 0 && cfg.SubagentTreeMaxToolRounds > 0 && tree.toolRounds >= cfg.SubagentTreeMaxToolRounds {
		treeSpent = true
	}
	svc.mu.Unlock()
	if rounds < 0 {
		rounds = 0
	}
	if treeSpent {
		return withRun(spawnResult(pb.SubagentStatus_SUBAGENT_STATUS_STOPPED, pb.SubagentExitReason_SUBAGENT_EXIT_REASON_BUDGET,
			"the tree-wide subagent budget is spent")), nil
	}

	cctx, cancel := context.WithDeadline(ctx, childDeadline(ctx, cfg, req.GetTimeoutMs()))
	defer cancel()

	// Concurrency per root: wait for a slot, then TIMEOUT.
	select {
	case tree.slots <- struct{}{}:
		defer func() { <-tree.slots }()
	case <-cctx.Done():
		return s.endOfCall(ctx, withRun(failedTimeout(fmt.Sprintf(
			"no subagent slot freed before the timeout (execution.agents.subagent_max_concurrency=%d)", cap(tree.slots)))))
	}

	// The child's own lease, bound before anything can present it.
	childLease, err := s.LLMGateway.Acquire(cctx, domain.StepAllocation{}, StepLeaseTokenLimit, StepLeaseDuration)
	if err != nil || childLease == "" {
		return withRun(failed(pb.SubagentExitReason_SUBAGENT_EXIT_REASON_ERROR, fmt.Sprintf("mint child lease: %v", err))), nil
	}
	s.leaseResolver().BindLease(childLease, domain.LeaseBinding{
		SessionID:          parent.SessionID,
		RunID:              parent.RunID,
		StepIndex:          parent.StepIndex,
		AgentID:            childAgent,
		ConversationID:     parent.ConversationID,
		OriginMessageID:    parent.OriginMessageID,
		ParentLeaseID:      lease,
		MemoryWriteGranted: memWrite,
	})
	child := &subagentChild{tree: tree, parent: lease, tools: tools, maxToolRounds: rounds, tokenBudget: budget}
	svc.mu.Lock()
	svc.children[childLease] = child
	svc.mu.Unlock()
	if s.ToolExecutor != nil {
		s.ToolExecutor.Overlay.Activate(string(childLease), overlay)
	}
	var usage llm.TokenUsage
	completed := false
	complete := func() {
		if completed {
			return
		}
		completed = true
		// Complete also clears the child's overlay (the gateway's completion hook).
		u, cerr := s.LLMGateway.Complete(context.WithoutCancel(ctx), childLease)
		if cerr == nil {
			usage = u
			if g, ok := s.LLMGateway.(subagentGateway); ok {
				g.RollUpLease(lease, u) // D1: child spend rolls up to the parent, so to the root
			}
		}
		svc.mu.Lock()
		delete(svc.children, childLease)
		if pc, ok := svc.children[lease]; ok && cerr == nil {
			pc.tokens += int64(u.TotalTokens) // a grandchild counts toward its parent's own budget
		}
		svc.mu.Unlock()
	}
	defer complete()

	names := make([]string, 0, len(tools))
	for n := range tools {
		names = append(names, n)
	}
	sort.Strings(names)
	h := s.subagentHandoff(req, parent, childLease, runID, depth, rounds, names, cfg)

	text, payloadType, winner, derr := s.dispatchSubagent(cctx, req, childAgent, h)
	if childAgent == "" {
		childAgent = winner
	}
	var structured string
	schema := []byte(req.GetOutputSchema().GetSchemaJson())
	if derr == nil && payloadType != "error" && len(schema) > 0 {
		var verr error
		structured, verr = llm.ValidateJSONReply(schema, text)
		if verr != nil && cctx.Err() == nil {
			// One repair turn (C1 semantics), on the same child lease.
			rh := s.subagentHandoff(req, parent, childLease, runID, depth, rounds, names, cfg)
			rh.Payload.Data = []byte(fmt.Sprintf("%s\n\nYour previous answer did not validate against the required output schema (%v). "+
				"Previous answer:\n%s\n\nReply with ONLY the corrected JSON.", req.GetObjective(), verr, text))
			target := childAgent
			rtext, rtype, _, rerr := s.dispatchSubagent(cctx, &pb.SpawnSubagentRequest{TargetAgentId: target}, target, rh)
			if rerr == nil && rtype != "error" {
				text = rtext
				structured, verr = llm.ValidateJSONReply(schema, text)
			} else {
				derr = rerr
			}
		}
		if derr == nil && verr != nil {
			complete()
			r := s.subagentResult(svc, child, usage, cfg, text, "", false)
			r.Status, r.ExitReason = pb.SubagentStatus_SUBAGENT_STATUS_FAILED, pb.SubagentExitReason_SUBAGENT_EXIT_REASON_ERROR
			r.Detail = "output failed the output_schema after one repair: " + verr.Error()
			return withRun(r), nil
		}
	}
	complete()

	svc.mu.Lock()
	budgetHit, denials := child.budgetHit, child.denials
	svc.mu.Unlock()
	r := s.subagentResult(svc, child, usage, cfg, text, structured, false)
	withRun(r)
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		return nil, status.Error(codes.Canceled, "spawn cancelled by the caller")
	case errors.Is(cctx.Err(), context.DeadlineExceeded):
		r.Status, r.ExitReason, r.Truncated = pb.SubagentStatus_SUBAGENT_STATUS_STOPPED, pb.SubagentExitReason_SUBAGENT_EXIT_REASON_TIMEOUT, true
		r.Detail = "the child ran past its timeout (timeout_ms, clamped to the parent's deadline)"
	case budgetHit:
		r.Status, r.ExitReason, r.Truncated = pb.SubagentStatus_SUBAGENT_STATUS_STOPPED, pb.SubagentExitReason_SUBAGENT_EXIT_REASON_BUDGET, true
		r.Detail = "the child hit its token budget, the tree-wide budget or its tool-round cap"
	case derr != nil:
		r.Status, r.ExitReason = pb.SubagentStatus_SUBAGENT_STATUS_FAILED, pb.SubagentExitReason_SUBAGENT_EXIT_REASON_ERROR
		r.Detail = derr.Error()
		if errors.Is(derr, ErrSubagentBudget) || errors.Is(derr, llm.ErrSpendCeiling) {
			r.Status, r.ExitReason, r.Truncated = pb.SubagentStatus_SUBAGENT_STATUS_STOPPED, pb.SubagentExitReason_SUBAGENT_EXIT_REASON_BUDGET, true
		}
	case payloadType == "error":
		r.Status, r.ExitReason = pb.SubagentStatus_SUBAGENT_STATUS_FAILED, pb.SubagentExitReason_SUBAGENT_EXIT_REASON_ERROR
		r.Detail = truncateRunes(text, 500)
		if denials > 0 {
			r.ExitReason = pb.SubagentExitReason_SUBAGENT_EXIT_REASON_DENIED
		}
	default:
		r.Status, r.ExitReason = pb.SubagentStatus_SUBAGENT_STATUS_DONE, pb.SubagentExitReason_SUBAGENT_EXIT_REASON_COMPLETED
	}
	if r.Truncated && r.Status == pb.SubagentStatus_SUBAGENT_STATUS_DONE {
		// A size cut on a completed answer: still done, and flagged.
		r.Detail = "the result was cut at execution.agents.subagent_result_max_chars"
	}
	slog.InfoContext(ctx, "ADR-0139 D12: subagent finished", "parent_agent", parent.AgentID, "child_agent", childAgent,
		"child_run_id", runID, "status", r.Status.String(), "exit_reason", r.ExitReason.String(), "tool_rounds", r.ToolRounds)
	return r, nil
}

// subagentHandoff builds the child's task: the objective as the payload, the
// child lease as its session token, and the D12 shape keys for the SDK.
func (s *Server) subagentHandoff(req *pb.SpawnSubagentRequest, parent domain.LeaseBinding, childLease domain.LeaseID,
	runID string, depth, rounds int, tools []string, cfg config.AgentsConfig) *domain.Handoff {
	mode := "isolated"
	if req.GetMode() == pb.SubagentMode_SUBAGENT_MODE_FORK {
		mode = "fork"
	}
	c := map[string]string{
		"task_id":                runID,
		"_step_index":            strconv.Itoa(parent.StepIndex),
		"original_prompt":        req.GetObjective(),
		"_session_token_id":      string(childLease),
		SubagentKeyMarker:        "1",
		SubagentKeyRunID:         runID,
		SubagentKeyMode:          mode,
		SubagentKeyDepth:         strconv.Itoa(depth),
		SubagentKeyMayspawn:      strconv.FormatBool(depth < cfg.SubagentMaxDepth),
		SubagentKeyMaxToolRounds: strconv.Itoa(rounds),
		SubagentKeyToolset:       strings.Join(tools, ","),
	}
	if sch := req.GetOutputSchema(); sch != nil && sch.GetSchemaJson() != "" {
		c[SubagentKeyOutputSchema] = sch.GetSchemaJson()
	}
	if mode == "fork" {
		if req.GetContextSummary() != "" {
			c[SubagentKeyContextSummary] = req.GetContextSummary()
		}
		if refs := req.GetContextRefs(); len(refs) > 0 {
			type ref struct {
				CID     string `json:"cid"`
				Type    string `json:"type,omitempty"`
				Snippet string `json:"snippet,omitempty"`
			}
			out := make([]ref, 0, len(refs))
			for _, r := range refs {
				out = append(out, ref{CID: r.GetCid(), Type: r.GetType(), Snippet: r.GetSnippet()})
			}
			if b, err := json.Marshal(out); err == nil {
				c[SubagentKeyContextRefs] = string(b)
			}
		}
	}
	return &domain.Handoff{
		FromAgent: parent.AgentID,
		Payload:   &domain.Payload{Type: "task", Data: []byte(req.GetObjective())},
		Context:   c,
	}
}

// dispatchSubagent runs the child through the ONE dispatch path: CallAgent for
// a known agent, Execute (selection) for a capability. Returns the answer text,
// the payload type, the agent that ran it and the dispatch error.
func (s *Server) dispatchSubagent(ctx context.Context, req *pb.SpawnSubagentRequest, agent string, h *domain.Handoff) (string, string, string, error) {
	ctx = domain.WithDedicatedInstance(ctx)
	var resp *domain.Handoff
	var err error
	if agent != "" {
		h.ToAgent = agent
		resp, err = s.Dispatcher.CallAgent(ctx, agent, h, "")
	} else {
		var res *domain.DispatchResult
		res, err = s.Dispatcher.Execute(ctx, &domain.DispatchTask{
			ID:                   h.Context["task_id"],
			Description:          req.GetObjective(),
			RequiredCapabilities: []string{req.GetTargetCapability()},
		}, h)
		if res != nil {
			resp = res.Handoff
		}
	}
	if err != nil {
		return "", "", agent, err
	}
	if resp == nil || resp.Payload == nil {
		return "", "", agent, errors.New("the child returned no result")
	}
	if agent == "" && resp.Context != nil {
		agent = resp.Context[domain.HandoffKeyWinnerAgentID]
	}
	return string(resp.Payload.Data), resp.Payload.Type, agent, nil
}

// subagentResult fills the parts of the response every exit shares: text and
// structured_json (cut at the configured cap), usage, rounds and artifacts.
func (s *Server) subagentResult(svc *SubagentService, child *subagentChild, u llm.TokenUsage, cfg config.AgentsConfig,
	text, structured string, truncated bool) *pb.SpawnSubagentResponse {
	limit := cfg.SubagentResultMaxChars
	if limit <= 0 {
		limit = 16000
	}
	t, cut1 := capRunes(text, limit)
	j := structured
	cut2 := false
	if utf8.RuneCountInString(structured) > limit {
		j, cut2 = "", true // a cut JSON document is not JSON: drop it, keep the text
	}
	svc.mu.Lock()
	rounds, artifacts := child.toolRounds, append([]string(nil), child.artifacts...)
	svc.mu.Unlock()
	return &pb.SpawnSubagentResponse{
		Text:           t,
		StructuredJson: j,
		Truncated:      truncated || cut1 || cut2,
		Artifacts:      artifacts,
		ToolRounds:     int32(rounds),
		Usage: &pb.GenerateUsage{
			InputTokens:     int32(u.PromptTokens),
			OutputTokens:    int32(u.CompletionTokens),
			CacheReadTokens: int32(u.CacheReadTokens),
			Estimated:       u.Estimated,
		},
	}
}

func capRunes(s string, n int) (string, bool) {
	if utf8.RuneCountInString(s) <= n {
		return s, false
	}
	r := []rune(s)
	return string(r[:n]), true
}

func truncateRunes(s string, n int) string {
	out, _ := capRunes(s, n)
	return out
}
