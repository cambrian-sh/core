package network

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
)

// ADR-0139 D12 kernel tests. The child is driven by a fake dispatcher whose
// "agent" runs against the REAL gateway, lease registry, ToolExecutor and
// SubagentService, so every limit is the kernel's own enforcement.

type childFn func(ctx context.Context, agentID string, h *domain.Handoff) (*domain.Handoff, error)

type spawnDispatcher struct {
	fn    childFn
	calls atomic.Int32
	// dedicated records whether every child call carried the dedicated-instance marker.
	dedicated atomic.Bool
	gw        *SubstrateLLMGateway
}

func (d *spawnDispatcher) CallAgent(ctx context.Context, agentID string, h *domain.Handoff, _ string) (*domain.Handoff, error) {
	d.calls.Add(1)
	d.dedicated.Store(domain.DedicatedInstanceRequested(ctx))
	return d.fn(ctx, agentID, h)
}

func (d *spawnDispatcher) Execute(ctx context.Context, task *domain.DispatchTask, h *domain.Handoff) (*domain.DispatchResult, error) {
	// Selection picks "cap-agent"; the real Dispatcher binds the lease to it.
	d.gw.BindLeaseAgent(domain.LeaseID(h.Context["_session_token_id"]), "cap-agent")
	resp, err := d.CallAgent(ctx, "cap-agent", h, "")
	if resp != nil {
		if resp.Context == nil {
			resp.Context = map[string]string{}
		}
		resp.Context[domain.HandoffKeyWinnerAgentID] = "cap-agent"
	}
	return &domain.DispatchResult{Handoff: resp}, err
}

func answer(text string) *domain.Handoff {
	return &domain.Handoff{Payload: &domain.Payload{Type: "text", Data: []byte(text)}}
}

type spawnFixture struct {
	s      *Server
	gw     *SubstrateLLMGateway
	d      *spawnDispatcher
	parent domain.LeaseID
	grants *domain.InMemoryGrantsStore
}

func defaultSubagentCfg() config.AgentsConfig {
	return config.DefaultConfig().Execution.Agents
}

func newSpawnFixture(t *testing.T, cfg config.AgentsConfig, hooks *domain.HookChain, fn childFn) *spawnFixture {
	t.Helper()
	s, gw := newLeaseServer(t)
	useFakeModel(gw, "llm:fake", textModel{})
	gw.SetDefaultModelID("llm:fake")
	reg := domain.NewInMemoryToolRegistry()
	for _, n := range []string{"web_fetch", "web_admin", "read_file", "shell", "clarify", "send_message"} {
		reg.Register(domain.SystemTool{Name: n})
	}
	grants := domain.NewInMemoryGrantsStore()
	grants.Set("parent-agent", []domain.ToolGrant{
		{Tool: "web_fetch", Policy: domain.ToolResourcePolicy{AllowAll: true}},
		{Tool: "read_file", Policy: domain.ToolResourcePolicy{AllowAll: true}},
		{Tool: "clarify", Policy: domain.ToolResourcePolicy{AllowAll: true}},
		{Tool: "send_message", Policy: domain.ToolResourcePolicy{AllowAll: true}},
	})
	svc := NewSubagentService(cfg, hooks)
	s.Subagents = svc
	s.ToolExecutor = &domain.ToolExecutor{Registry: reg, Grants: grants, LeaseCeiling: svc,
		Overlay: domain.NewRunGrantOverlay(), Handler: okTool{}}
	gw.SetLeaseGate(svc)
	d := &spawnDispatcher{fn: fn, gw: gw}
	s.Dispatcher = d
	parent, _ := gw.Acquire(context.Background(), domain.StepAllocation{}, 4096, time.Minute)
	gw.BindLease(parent, domain.LeaseBinding{SessionID: "sess-1", RunID: "run-1", StepIndex: 2, AgentID: "parent-agent"})
	return &spawnFixture{s: s, gw: gw, d: d, parent: parent, grants: grants}
}

// okTool runs every tool successfully.
type okTool struct{}

func (okTool) Execute(context.Context, domain.ToolCall) ([]byte, error) {
	return []byte(`{"ok":true}`), nil
}

func (f *spawnFixture) spawn(t *testing.T, req *pb.SpawnSubagentRequest) (*pb.SpawnSubagentResponse, error) {
	t.Helper()
	if req.LeaseId == "" {
		req.LeaseId = string(f.parent)
	}
	return f.s.SpawnSubagent(agentPlaneCtx("parent-agent"), req)
}

func childLease(h *domain.Handoff) domain.LeaseID {
	return domain.LeaseID(h.Context["_session_token_id"])
}

// childTool calls ExecuteTool as the child would, over its own lease.
func (f *spawnFixture) childTool(agentID string, lease domain.LeaseID, tool string) *pb.ExecuteToolResponse {
	resp, _ := f.s.ExecuteTool(agentPlaneCtx(agentID, "x-agent-id", agentID), &pb.ExecuteToolRequest{ToolName: tool, LeaseId: string(lease), ArgsJson: "{}"})
	return resp
}

func wantExit(t *testing.T, r *pb.SpawnSubagentResponse, err error, st pb.SubagentStatus, reason pb.SubagentExitReason) {
	t.Helper()
	if err != nil {
		t.Fatalf("spawn returned an RPC error, want an OK result: %v", err)
	}
	if r.GetStatus() != st || r.GetExitReason() != reason {
		t.Fatalf("got %v/%v (detail %q), want %v/%v", r.GetStatus(), r.GetExitReason(), r.GetDetail(), st, reason)
	}
}

func TestSpawn_Completes_OnTheParentsProfile_WithADedicatedInstance(t *testing.T) {
	t.Parallel()
	var seen *domain.Handoff
	var agent string
	f := newSpawnFixture(t, defaultSubagentCfg(), nil, func(_ context.Context, a string, h *domain.Handoff) (*domain.Handoff, error) {
		seen, agent = h, a
		return answer("child says hi"), nil
	})
	r, err := f.spawn(t, &pb.SpawnSubagentRequest{Objective: "summarise", Toolset: []string{"web_fetch"}})
	wantExit(t, r, err, pb.SubagentStatus_SUBAGENT_STATUS_DONE, pb.SubagentExitReason_SUBAGENT_EXIT_REASON_COMPLETED)
	if r.Text != "child says hi" || r.ChildRunId == "" {
		t.Fatalf("result: %+v", r)
	}
	if agent != "parent-agent" {
		t.Errorf("empty target must run the parent's own profile, ran %q", agent)
	}
	if !f.d.dedicated.Load() {
		t.Error("the child dispatch must carry the dedicated-instance marker (agents are single-threaded)")
	}
	if seen.Context[SubagentKeyToolset] != "web_fetch" || seen.Context[SubagentKeyMayspawn] != "false" ||
		seen.Context[SubagentKeyDepth] != "1" || childLease(seen) == f.parent {
		t.Errorf("child handoff: %+v", seen.Context)
	}
	if f.gw.SessionCount() != 1 {
		t.Errorf("the child lease must be completed when the spawn returns; %d leases live", f.gw.SessionCount())
	}
}

func TestSpawn_ToolsetOutsideParent_IsDenied_NotNarrowed(t *testing.T) {
	t.Parallel()
	for name, toolset := range map[string][]string{
		"ungranted name":                  {"web_fetch", "shell"},
		"glob reaching an ungranted tool": {"web_*"},
		"unknown tool":                    {"no_such_tool"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newSpawnFixture(t, defaultSubagentCfg(), nil, func(context.Context, string, *domain.Handoff) (*domain.Handoff, error) {
				return answer("ran"), nil
			})
			r, err := f.spawn(t, &pb.SpawnSubagentRequest{Objective: "x", Toolset: toolset})
			wantExit(t, r, err, pb.SubagentStatus_SUBAGENT_STATUS_FAILED, pb.SubagentExitReason_SUBAGENT_EXIT_REASON_DENIED)
			if f.d.calls.Load() != 0 {
				t.Error("a denied spawn must never run the child")
			}
		})
	}
}

func TestSpawn_StarIsParentsToolsMinusWithheld_UnlessNamed(t *testing.T) {
	t.Parallel()
	var toolsets []string
	var mu sync.Mutex
	f := newSpawnFixture(t, defaultSubagentCfg(), nil, func(_ context.Context, _ string, h *domain.Handoff) (*domain.Handoff, error) {
		mu.Lock()
		toolsets = append(toolsets, h.Context[SubagentKeyToolset])
		mu.Unlock()
		return answer("ok"), nil
	})
	r, err := f.spawn(t, &pb.SpawnSubagentRequest{Objective: "x", Toolset: []string{"*"}})
	wantExit(t, r, err, pb.SubagentStatus_SUBAGENT_STATUS_DONE, pb.SubagentExitReason_SUBAGENT_EXIT_REASON_COMPLETED)
	r, err = f.spawn(t, &pb.SpawnSubagentRequest{Objective: "x", Toolset: []string{"*", "clarify"}})
	wantExit(t, r, err, pb.SubagentStatus_SUBAGENT_STATUS_DONE, pb.SubagentExitReason_SUBAGENT_EXIT_REASON_COMPLETED)
	r, err = f.spawn(t, &pb.SpawnSubagentRequest{Objective: "x"})
	wantExit(t, r, err, pb.SubagentStatus_SUBAGENT_STATUS_DONE, pb.SubagentExitReason_SUBAGENT_EXIT_REASON_COMPLETED)
	want := []string{"read_file,web_fetch", "clarify,read_file,web_fetch", ""}
	for i, w := range want {
		if toolsets[i] != w {
			t.Errorf("spawn %d: toolset %q, want %q", i, toolsets[i], w)
		}
	}
}

func TestSpawn_ChildToolCeiling_EnforcedByToolExecutor_EvenUnrestricted(t *testing.T) {
	t.Parallel()
	var inside, outside *pb.ExecuteToolResponse
	var menu []string
	var f *spawnFixture
	f = newSpawnFixture(t, defaultSubagentCfg(), nil, func(_ context.Context, a string, h *domain.Handoff) (*domain.Handoff, error) {
		inside = f.childTool(a, childLease(h), "web_fetch")
		outside = f.childTool(a, childLease(h), "read_file")
		lt, _ := f.s.ListTools(agentPlaneCtx(a, "x-agent-id", a, leaseHeader, string(childLease(h))), &pb.ListToolsRequest{})
		for _, d := range lt.GetTools() {
			menu = append(menu, d.Name)
		}
		return answer("ok"), nil
	})
	f.s.ToolExecutor.Unrestricted = true
	r, err := f.spawn(t, &pb.SpawnSubagentRequest{Objective: "x", Toolset: []string{"web_fetch"}})
	wantExit(t, r, err, pb.SubagentStatus_SUBAGENT_STATUS_DONE, pb.SubagentExitReason_SUBAGENT_EXIT_REASON_COMPLETED)
	if inside.GetDenied() {
		t.Errorf("a toolset tool was denied: %+v", inside)
	}
	if !outside.GetDenied() {
		t.Error("a tool outside the child's toolset ran (the lease ceiling must hold under tools_unrestricted)")
	}
	if strings.Join(menu, ",") != "web_fetch" {
		t.Errorf("child menu %v, want [web_fetch]", menu)
	}
	if r.ToolRounds != 2 {
		t.Errorf("tool_rounds %d, want 2", r.ToolRounds)
	}
}

func TestSpawn_DepthComesFromTheLeaseChain(t *testing.T) {
	t.Parallel()
	var nested *pb.SpawnSubagentResponse
	var f *spawnFixture
	f = newSpawnFixture(t, defaultSubagentCfg(), nil, func(_ context.Context, a string, h *domain.Handoff) (*domain.Handoff, error) {
		// The child tries to spawn a grandchild with its own lease.
		nested, _ = f.s.SpawnSubagent(agentPlaneCtx(a), &pb.SpawnSubagentRequest{LeaseId: string(childLease(h)), Objective: "deeper"})
		return answer("ok"), nil
	})
	r, err := f.spawn(t, &pb.SpawnSubagentRequest{Objective: "x"})
	wantExit(t, r, err, pb.SubagentStatus_SUBAGENT_STATUS_DONE, pb.SubagentExitReason_SUBAGENT_EXIT_REASON_COMPLETED)
	wantExit(t, nested, nil, pb.SubagentStatus_SUBAGENT_STATUS_FAILED, pb.SubagentExitReason_SUBAGENT_EXIT_REASON_DEPTH)
	if f.d.calls.Load() != 1 {
		t.Errorf("the grandchild must never run; %d dispatches", f.d.calls.Load())
	}

	// A lease the kernel bound as a child is depth 1 whatever the caller claims;
	// there is no request field that could lower it.
	forged, _ := f.gw.Acquire(context.Background(), domain.StepAllocation{}, 4096, time.Minute)
	f.gw.BindLease(forged, domain.LeaseBinding{SessionID: "sess-1", AgentID: "parent-agent", ParentLeaseID: f.parent})
	r, err = f.spawn(t, &pb.SpawnSubagentRequest{LeaseId: string(forged), Objective: "x"})
	wantExit(t, r, err, pb.SubagentStatus_SUBAGENT_STATUS_FAILED, pb.SubagentExitReason_SUBAGENT_EXIT_REASON_DEPTH)

	// Depth 2 allows exactly one more hop.
	cfg := defaultSubagentCfg()
	cfg.SubagentMaxDepth = 2
	var nested2 *pb.SpawnSubagentResponse
	var f2 *spawnFixture
	f2 = newSpawnFixture(t, cfg, nil, func(_ context.Context, a string, h *domain.Handoff) (*domain.Handoff, error) {
		if h.Context[SubagentKeyDepth] == "1" {
			nested2, _ = f2.s.SpawnSubagent(agentPlaneCtx(a), &pb.SpawnSubagentRequest{LeaseId: string(childLease(h)), Objective: "deeper"})
		}
		return answer("ok"), nil
	})
	r, err = f2.spawn(t, &pb.SpawnSubagentRequest{Objective: "x"})
	wantExit(t, r, err, pb.SubagentStatus_SUBAGENT_STATUS_DONE, pb.SubagentExitReason_SUBAGENT_EXIT_REASON_COMPLETED)
	wantExit(t, nested2, nil, pb.SubagentStatus_SUBAGENT_STATUS_DONE, pb.SubagentExitReason_SUBAGENT_EXIT_REASON_COMPLETED)
}

func TestSpawn_ConcurrencyCap_WaitsThenTimesOut(t *testing.T) {
	t.Parallel()
	cfg := defaultSubagentCfg()
	cfg.SubagentMaxConcurrency = 1
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f := newSpawnFixture(t, cfg, nil, func(context.Context, string, *domain.Handoff) (*domain.Handoff, error) {
		once.Do(func() { close(started) })
		<-release
		return answer("first"), nil
	})
	var wg sync.WaitGroup
	wg.Add(1)
	var first *pb.SpawnSubagentResponse
	go func() {
		defer wg.Done()
		first, _ = f.spawn(t, &pb.SpawnSubagentRequest{Objective: "slow"})
	}()
	<-started
	r, err := f.spawn(t, &pb.SpawnSubagentRequest{Objective: "second", TimeoutMs: 50})
	wantExit(t, r, err, pb.SubagentStatus_SUBAGENT_STATUS_STOPPED, pb.SubagentExitReason_SUBAGENT_EXIT_REASON_TIMEOUT)
	close(release)
	wg.Wait()
	if first.GetExitReason() != pb.SubagentExitReason_SUBAGENT_EXIT_REASON_COMPLETED {
		t.Errorf("first spawn: %+v", first)
	}
	if f.d.calls.Load() != 1 {
		t.Errorf("the timed-out spawn must not have run; %d dispatches", f.d.calls.Load())
	}
}

func TestSpawn_TreeWideTokenBudget_StopsTheChild_AndLaterSpawns(t *testing.T) {
	t.Parallel()
	cfg := defaultSubagentCfg()
	cfg.SubagentTreeTokenBudget = 100
	var genErr error
	var f *spawnFixture
	f = newSpawnFixture(t, cfg, nil, func(ctx context.Context, _ string, h *domain.Handoff) (*domain.Handoff, error) {
		// The child's calls so far cost 150 tokens (a retrieval chain charged back).
		f.gw.ChargeLease(childLease(h), llm.TokenUsage{TotalTokens: 150})
		out := make(chan domain.StreamChunk, 8)
		genErr = f.gw.StreamChunks(ctx, childLease(h), domain.GenerateRequest{}, out)
		return answer("partial"), nil
	})
	// token_budget 10000 is clamped to the 100 the tree has.
	r, err := f.spawn(t, &pb.SpawnSubagentRequest{Objective: "x", TokenBudget: 10000})
	wantExit(t, r, err, pb.SubagentStatus_SUBAGENT_STATUS_STOPPED, pb.SubagentExitReason_SUBAGENT_EXIT_REASON_BUDGET)
	if !errors.Is(genErr, ErrSubagentBudget) || !r.Truncated || r.Text != "partial" {
		t.Fatalf("gen err %v, result %+v", genErr, r)
	}
	// The child's spend rolled up to the parent lease (D1: no per-child budget escapes the root).
	if st := f.gw.GetBudgetLeaseState(f.parent); st == nil || st.ActualTokensUsed != 150 {
		t.Errorf("parent lease usage: %+v", st)
	}
	r, err = f.spawn(t, &pb.SpawnSubagentRequest{Objective: "again"})
	wantExit(t, r, err, pb.SubagentStatus_SUBAGENT_STATUS_STOPPED, pb.SubagentExitReason_SUBAGENT_EXIT_REASON_BUDGET)
	if f.d.calls.Load() != 1 {
		t.Error("a spawn on a spent tree must not run")
	}
}

func TestSpawn_ToolRoundCap_IsKernelEnforced(t *testing.T) {
	t.Parallel()
	var second *pb.ExecuteToolResponse
	var f *spawnFixture
	f = newSpawnFixture(t, defaultSubagentCfg(), nil, func(_ context.Context, a string, h *domain.Handoff) (*domain.Handoff, error) {
		f.childTool(a, childLease(h), "web_fetch")
		second = f.childTool(a, childLease(h), "web_fetch")
		return answer("done-ish"), nil
	})
	r, err := f.spawn(t, &pb.SpawnSubagentRequest{Objective: "x", Toolset: []string{"web_fetch"}, MaxToolRounds: 1})
	wantExit(t, r, err, pb.SubagentStatus_SUBAGENT_STATUS_STOPPED, pb.SubagentExitReason_SUBAGENT_EXIT_REASON_BUDGET)
	if !second.GetDenied() || r.ToolRounds != 1 {
		t.Errorf("second call %+v, rounds %d", second, r.ToolRounds)
	}
}

func TestSpawn_CancellingTheCall_CancelsTheChild(t *testing.T) {
	t.Parallel()
	childSaw := make(chan error, 1)
	started := make(chan struct{})
	f := newSpawnFixture(t, defaultSubagentCfg(), nil, func(ctx context.Context, _ string, _ *domain.Handoff) (*domain.Handoff, error) {
		close(started)
		<-ctx.Done()
		childSaw <- ctx.Err()
		return nil, ctx.Err()
	})
	ctx, cancel := context.WithCancel(agentPlaneCtx("parent-agent"))
	go func() { <-started; cancel() }()
	_, err := f.s.SpawnSubagent(ctx, &pb.SpawnSubagentRequest{LeaseId: string(f.parent), Objective: "x"})
	if status.Code(err) != codes.Canceled {
		t.Fatalf("want Canceled, got %v", err)
	}
	if e := <-childSaw; !errors.Is(e, context.Canceled) {
		t.Errorf("the child's ctx was not cancelled: %v", e)
	}
	if f.gw.SessionCount() != 1 {
		t.Errorf("the child lease must be completed on cancel; %d leases live", f.gw.SessionCount())
	}
}

func TestSpawn_ClientSpawnID_MakesRetriesIdempotent(t *testing.T) {
	t.Parallel()
	f := newSpawnFixture(t, defaultSubagentCfg(), nil, func(context.Context, string, *domain.Handoff) (*domain.Handoff, error) {
		return answer("once"), nil
	})
	a, err := f.spawn(t, &pb.SpawnSubagentRequest{Objective: "x", ClientSpawnId: "k1"})
	wantExit(t, a, err, pb.SubagentStatus_SUBAGENT_STATUS_DONE, pb.SubagentExitReason_SUBAGENT_EXIT_REASON_COMPLETED)
	b, err := f.spawn(t, &pb.SpawnSubagentRequest{Objective: "x", ClientSpawnId: "k1"})
	wantExit(t, b, err, pb.SubagentStatus_SUBAGENT_STATUS_DONE, pb.SubagentExitReason_SUBAGENT_EXIT_REASON_COMPLETED)
	if f.d.calls.Load() != 1 || a.ChildRunId != b.ChildRunId || b.Text != "once" {
		t.Errorf("retry re-ran the child: calls %d, %q vs %q", f.d.calls.Load(), a.ChildRunId, b.ChildRunId)
	}
	if _, err := f.spawn(t, &pb.SpawnSubagentRequest{Objective: "x", ClientSpawnId: "k2"}); err != nil || f.d.calls.Load() != 2 {
		t.Errorf("a new key must spawn: %v, calls %d", err, f.d.calls.Load())
	}
}

func TestSpawn_BadLease_IsPermissionDenied(t *testing.T) {
	t.Parallel()
	f := newSpawnFixture(t, defaultSubagentCfg(), nil, func(context.Context, string, *domain.Handoff) (*domain.Handoff, error) {
		return answer("x"), nil
	})
	other, _ := f.gw.Acquire(context.Background(), domain.StepAllocation{}, 4096, time.Minute)
	f.gw.BindLease(other, domain.LeaseBinding{SessionID: "s2", AgentID: "someone-else"})
	for name, lease := range map[string]string{"missing": "", "unknown": "lease-deadbeef", "foreign": string(other)} {
		_, err := f.s.SpawnSubagent(agentPlaneCtx("parent-agent"), &pb.SpawnSubagentRequest{LeaseId: lease, Objective: "x"})
		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s lease: got %v, want PermissionDenied", name, err)
		}
	}
}

func TestSpawn_MalformedRequests_AreInvalidArgument(t *testing.T) {
	t.Parallel()
	f := newSpawnFixture(t, defaultSubagentCfg(), nil, func(context.Context, string, *domain.Handoff) (*domain.Handoff, error) {
		return answer("x"), nil
	})
	for name, req := range map[string]*pb.SpawnSubagentRequest{
		"empty objective":         {},
		"both targets":            {Objective: "x", TargetAgentId: "a", TargetCapability: "c"},
		"summary on isolated":     {Objective: "x", Mode: pb.SubagentMode_SUBAGENT_MODE_ISOLATED, ContextSummary: "s"},
		"summary on default mode": {Objective: "x", ContextSummary: "s"},
		"bad schema":              {Objective: "x", OutputSchema: &pb.ResponseSchema{SchemaJson: "{not json"}},
		"negative timeout":        {Objective: "x", TimeoutMs: -1},
	} {
		if _, err := f.spawn(t, req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: got %v, want InvalidArgument", name, err)
		}
	}
}

func TestSpawn_Fork_CarriesTheContextSummary(t *testing.T) {
	t.Parallel()
	var seen *domain.Handoff
	f := newSpawnFixture(t, defaultSubagentCfg(), nil, func(_ context.Context, _ string, h *domain.Handoff) (*domain.Handoff, error) {
		seen = h
		return answer("ok"), nil
	})
	r, err := f.spawn(t, &pb.SpawnSubagentRequest{Objective: "x", Mode: pb.SubagentMode_SUBAGENT_MODE_FORK,
		ContextSummary: "## Goal\nfind it", ContextRefs: []*pb.ContextRef{{Cid: "abc"}}})
	wantExit(t, r, err, pb.SubagentStatus_SUBAGENT_STATUS_DONE, pb.SubagentExitReason_SUBAGENT_EXIT_REASON_COMPLETED)
	if seen.Context[SubagentKeyMode] != "fork" || seen.Context[SubagentKeyContextSummary] != "## Goal\nfind it" ||
		!strings.Contains(seen.Context[SubagentKeyContextRefs], `"abc"`) {
		t.Errorf("fork handoff: %+v", seen.Context)
	}
}

func TestSpawn_OutputSchema_ValidatesWithOneRepair(t *testing.T) {
	t.Parallel()
	schema := `{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`
	replies := []string{"not json", `{"n": 3}`}
	var i atomic.Int32
	f := newSpawnFixture(t, defaultSubagentCfg(), nil, func(context.Context, string, *domain.Handoff) (*domain.Handoff, error) {
		return answer(replies[i.Add(1)-1]), nil
	})
	r, err := f.spawn(t, &pb.SpawnSubagentRequest{Objective: "count", OutputSchema: &pb.ResponseSchema{SchemaJson: schema}})
	wantExit(t, r, err, pb.SubagentStatus_SUBAGENT_STATUS_DONE, pb.SubagentExitReason_SUBAGENT_EXIT_REASON_COMPLETED)
	if r.StructuredJson != `{"n": 3}` {
		t.Errorf("structured_json %q", r.StructuredJson)
	}
	replies = []string{"nope", "still nope"}
	i.Store(0)
	r, err = f.spawn(t, &pb.SpawnSubagentRequest{Objective: "count", OutputSchema: &pb.ResponseSchema{SchemaJson: schema}})
	wantExit(t, r, err, pb.SubagentStatus_SUBAGENT_STATUS_FAILED, pb.SubagentExitReason_SUBAGENT_EXIT_REASON_ERROR)
}

func TestSpawn_ResultIsCappedAndMarkedTruncated(t *testing.T) {
	t.Parallel()
	cfg := defaultSubagentCfg()
	cfg.SubagentResultMaxChars = 5
	f := newSpawnFixture(t, cfg, nil, func(context.Context, string, *domain.Handoff) (*domain.Handoff, error) {
		return answer("0123456789"), nil
	})
	r, err := f.spawn(t, &pb.SpawnSubagentRequest{Objective: "x"})
	wantExit(t, r, err, pb.SubagentStatus_SUBAGENT_STATUS_DONE, pb.SubagentExitReason_SUBAGENT_EXIT_REASON_COMPLETED)
	if r.Text != "01234" || !r.Truncated {
		t.Errorf("result %+v", r)
	}
}

func TestSpawn_CapabilityTarget_GoesThroughSelection(t *testing.T) {
	t.Parallel()
	var ran string
	f := newSpawnFixture(t, defaultSubagentCfg(), nil, func(_ context.Context, a string, _ *domain.Handoff) (*domain.Handoff, error) {
		ran = a
		return answer("ok"), nil
	})
	r, err := f.spawn(t, &pb.SpawnSubagentRequest{Objective: "x", TargetCapability: "research"})
	wantExit(t, r, err, pb.SubagentStatus_SUBAGENT_STATUS_DONE, pb.SubagentExitReason_SUBAGENT_EXIT_REASON_COMPLETED)
	if ran != "cap-agent" {
		t.Errorf("capability target ran %q", ran)
	}
}

type denyAgentAuthz struct{ domain.AllowAllAuthorizer }

func (denyAgentAuthz) Authorize(_ context.Context, req domain.AccessRequest) domain.AccessDecision {
	if req.Resource.Kind == domain.KindAgent {
		return domain.AccessDecision{Allowed: false, Detail: "not yours"}
	}
	return domain.AccessDecision{Allowed: true}
}

func TestSpawn_TargetThePrincipalMayNotDispatchTo_IsDenied(t *testing.T) {
	t.Parallel()
	f := newSpawnFixture(t, defaultSubagentCfg(), nil, func(context.Context, string, *domain.Handoff) (*domain.Handoff, error) {
		return answer("x"), nil
	})
	f.s.Authz = denyAgentAuthz{}
	r, err := f.spawn(t, &pb.SpawnSubagentRequest{Objective: "x", TargetAgentId: "secret-agent"})
	wantExit(t, r, err, pb.SubagentStatus_SUBAGENT_STATUS_FAILED, pb.SubagentExitReason_SUBAGENT_EXIT_REASON_DENIED)
}

func TestSpawn_MemoryWrite_GrantedOnlyWhenNamed(t *testing.T) {
	t.Parallel()
	var bindings []domain.LeaseBinding
	var f *spawnFixture
	f = newSpawnFixture(t, defaultSubagentCfg(), nil, func(_ context.Context, _ string, h *domain.Handoff) (*domain.Handoff, error) {
		b, _ := f.gw.ResolveLease(childLease(h))
		bindings = append(bindings, b)
		return answer("ok"), nil
	})
	_, _ = f.spawn(t, &pb.SpawnSubagentRequest{Objective: "x", Toolset: []string{"*"}})
	_, _ = f.spawn(t, &pb.SpawnSubagentRequest{Objective: "x", Toolset: []string{"propose_memory_note"}})
	if len(bindings) != 2 || bindings[0].MemoryWriteGranted || !bindings[1].MemoryWriteGranted ||
		bindings[0].ParentLeaseID != f.parent || bindings[0].SessionID != "sess-1" || bindings[0].RunID != "run-1" {
		t.Errorf("child bindings: %+v", bindings)
	}
}

// recordingHook records every subagent point; deny makes subagent_start refuse.
type recordingHook struct {
	mu     sync.Mutex
	points []domain.HookPoint
	deny   bool
}

func (h *recordingHook) Spec() domain.HookSpec {
	return domain.HookSpec{Name: "rec", Points: []domain.HookPoint{domain.HookSubagentStart, domain.HookSubagentEnd}}
}

func (h *recordingHook) Run(_ context.Context, ev domain.HookEvent) (domain.HookResult, error) {
	h.mu.Lock()
	h.points = append(h.points, ev.Point)
	h.mu.Unlock()
	if h.deny && ev.Point == domain.HookSubagentStart {
		return domain.HookResult{Verdict: domain.HookDeny, Reason: "no children today"}, nil
	}
	return domain.HookResult{}, nil
}

func TestSpawn_FiresSubagentHooks_AndAStartDenyRefuses(t *testing.T) {
	t.Parallel()
	h := &recordingHook{}
	chain, err := domain.NewHookChain([]domain.Hook{h}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := newSpawnFixture(t, defaultSubagentCfg(), chain, func(context.Context, string, *domain.Handoff) (*domain.Handoff, error) {
		return answer("ok"), nil
	})
	r, err := f.spawn(t, &pb.SpawnSubagentRequest{Objective: "x"})
	wantExit(t, r, err, pb.SubagentStatus_SUBAGENT_STATUS_DONE, pb.SubagentExitReason_SUBAGENT_EXIT_REASON_COMPLETED)
	if len(h.points) != 2 || h.points[0] != domain.HookSubagentStart || h.points[1] != domain.HookSubagentEnd {
		t.Fatalf("hook points %v", h.points)
	}
	h.deny = true
	r, err = f.spawn(t, &pb.SpawnSubagentRequest{Objective: "x"})
	wantExit(t, r, err, pb.SubagentStatus_SUBAGENT_STATUS_FAILED, pb.SubagentExitReason_SUBAGENT_EXIT_REASON_DENIED)
	if !strings.Contains(r.Detail, "no children today") || f.d.calls.Load() != 1 {
		t.Errorf("deny detail %q, calls %d", r.Detail, f.d.calls.Load())
	}
}

func TestSpawn_Unimplemented_WithoutTheService(t *testing.T) {
	t.Parallel()
	s, _ := newLeaseServer(t)
	if _, err := s.SpawnSubagent(context.Background(), &pb.SpawnSubagentRequest{Objective: "x"}); status.Code(err) != codes.Unimplemented {
		t.Errorf("got %v", err)
	}
}

// Not parallel: goleak sees the whole process.
func TestSpawn_LeaksNoGoroutines(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	cfg := defaultSubagentCfg()
	cfg.SubagentMaxConcurrency = 1
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	f := newSpawnFixture(t, cfg, nil, func(ctx context.Context, _ string, _ *domain.Handoff) (*domain.Handoff, error) {
		once.Do(func() { close(started) })
		select {
		case <-release:
		case <-ctx.Done():
		}
		return answer("ok"), nil
	})
	done := make(chan struct{})
	go func() { defer close(done); _, _ = f.spawn(t, &pb.SpawnSubagentRequest{Objective: "a"}) }()
	<-started
	_, _ = f.spawn(t, &pb.SpawnSubagentRequest{Objective: "b", TimeoutMs: 20})
	close(release)
	<-done
}
