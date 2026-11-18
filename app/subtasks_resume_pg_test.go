package app

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
	"github.com/cambrian-sh/core/internal/subtasks"
	"github.com/cambrian-sh/core/internal/testing/pgscratch"
)

// ENG-1760 end to end through the production path, on Postgres: the executor
// built from config over the ADR-0154 action record (buildActionRecordStore,
// buildExecutorLeases), run_subtasks wired by wireSubtasks, each subtask run by
// the agent-plane server (Server.RunSubtask: child lease, composition, the
// handoff to the composed runtime). Only the agent process (the dispatcher)
// and the LLM gateway's lease book are stand-ins.

// subtaskE2EGateway is the gateway's lease book: Acquire mints child leases,
// BindLease/ResolveLease keep their identity (the server resolves the parent's).
type subtaskE2EGateway struct {
	mu       sync.Mutex
	n        int
	bindings map[domain.LeaseID]domain.LeaseBinding
}

func (g *subtaskE2EGateway) Acquire(context.Context, domain.StepAllocation, int, time.Duration) (domain.LeaseID, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	return domain.LeaseID("e2e-child-" + string(rune('a'+g.n))), nil
}
func (g *subtaskE2EGateway) Complete(context.Context, domain.LeaseID) (llm.TokenUsage, error) {
	return llm.TokenUsage{}, nil
}
func (g *subtaskE2EGateway) EvictExpired() {}
func (g *subtaskE2EGateway) StreamChunks(context.Context, domain.LeaseID, domain.GenerateRequest, chan<- domain.StreamChunk) error {
	return nil
}
func (g *subtaskE2EGateway) GenerateWithTools(context.Context, domain.LeaseID, domain.GenerateRequest) (domain.GenerateResponse, error) {
	return domain.GenerateResponse{}, nil
}
func (g *subtaskE2EGateway) BindLease(id domain.LeaseID, b domain.LeaseBinding) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.bindings[id] = b
}
func (g *subtaskE2EGateway) ResolveLease(id domain.LeaseID) (domain.LeaseBinding, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	b, ok := g.bindings[id]
	return b, ok
}

// subtaskAgent is the composed runtime's process: it answers each subtask
// handoff by its brief.
type subtaskAgent struct {
	mu       sync.Mutex
	handoffs []*domain.Handoff
	answer   func(ctx context.Context, h *domain.Handoff) (*domain.Handoff, error)
}

func (a *subtaskAgent) Execute(context.Context, *domain.DispatchTask, *domain.Handoff) (*domain.DispatchResult, error) {
	panic("unused")
}
func (a *subtaskAgent) CallAgent(ctx context.Context, _ string, h *domain.Handoff, _ string) (*domain.Handoff, error) {
	a.mu.Lock()
	a.handoffs = append(a.handoffs, h)
	a.mu.Unlock()
	return a.answer(ctx, h)
}
func (a *subtaskAgent) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.handoffs)
}

const e2eSubtaskRun = "run-eng1760-e2e"

type subtaskKernel struct {
	exec   *domain.ToolExecutor
	server *subnetwork.Server
}

// bootSubtaskKernel is one kernel process's subtask stack over the database:
// a fresh pool, record store, registry, executor and server — nothing shared
// with an earlier boot but Postgres.
func bootSubtaskKernel(t *testing.T, dsn string, cfg config.ExecutionConfig, agent *subtaskAgent) *subtaskKernel {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	actions := buildActionRecordStore(ctx, pool)
	if actions == nil {
		t.Fatal("action_records missing on a migrated database")
	}
	l := buildExecutorLeases(cfg, actions)
	reg := domain.NewInMemoryToolRegistry()
	reg.Register(domain.SystemTool{Name: "crm_search", Effects: []domain.ToolEffect{domain.EffectRead}})
	te := newTestToolExecutor(t, l.controls(domain.ToolExecutorControls{Registry: reg}),
		domain.ToolExecutor{Overlay: domain.NewRunGrantOverlay(), InlineThreshold: 1 << 20})
	gw := &subtaskE2EGateway{bindings: map[domain.LeaseID]domain.LeaseBinding{
		"e2e-parent": {SessionID: "ses-eng1760", RunID: e2eSubtaskRun, StepIndex: 0, AgentID: "composed_worker"},
	}}
	srv := &subnetwork.Server{ToolExecutor: te, Compositions: l.composed, ComposedRuntimeID: "composed_worker",
		LLMGateway: gw, Dispatcher: agent, ExecCfg: cfg}
	wireSubtasks(cfg, srv, l.subtasks)
	if srv.Subtasks == nil {
		t.Fatal("run_subtasks not wired")
	}
	srv.RegisterSubtaskParent("e2e-parent", "composed_worker")
	return &subtaskKernel{exec: te, server: srv}
}

func (k *subtaskKernel) runSubtasks(t *testing.T, briefs ...map[string]any) []subtasks.Result {
	t.Helper()
	args, _ := json.Marshal(map[string]any{"subtasks": briefs})
	ctx := domain.WithLedgerProvenance(context.Background(), domain.LedgerProvenance{RunID: e2eSubtaskRun, StepIndex: 0, SessionID: "ses-eng1760"})
	resp := k.exec.Execute(ctx, domain.ToolCallRequest{AgentID: "composed_worker", ToolName: subtasks.ToolName,
		ArgsJSON: args, SessionTokenID: "e2e-parent"})
	if resp.Error != "" || resp.Denied {
		t.Fatalf("run_subtasks: error=%q denied=%q", resp.Error, resp.DenyReason)
	}
	var out struct {
		Subtasks []subtasks.Result `json:"subtasks"`
	}
	if err := json.Unmarshal(resp.ResultJSON, &out); err != nil {
		t.Fatalf("result %s: %v", resp.ResultJSON, err)
	}
	return out.Subtasks
}

func e2eBrief(id string) map[string]any {
	return map[string]any{"id": id, "objective": "list the open invoices of " + id,
		"expect": map[string]any{"shape": "list", "describe": "one row per invoice"}}
}

func cleanSubtaskRecords(t *testing.T, dsn string) {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(context.Background(), `DELETE FROM action_records WHERE run_id = $1`, e2eSubtaskRun); err != nil {
		t.Fatal(err)
	}
}

func TestPg_RunSubtasks_FinishedSubtaskSurvivesAKernelRestart(t *testing.T) {
	dsn, _ := pgscratch.Migrated(t)
	cleanSubtaskRecords(t, dsn)
	t.Cleanup(func() { cleanSubtaskRecords(t, dsn) })
	cfg := singleAgentComposed()

	// Kernel A: "acme" finishes, "zeta" fails (its approval expired).
	agentA := &subtaskAgent{answer: func(_ context.Context, h *domain.Handoff) (*domain.Handoff, error) {
		if strings.Contains(string(h.Payload.Data), "zeta") {
			return &domain.Handoff{Payload: &domain.Payload{Type: "error", Data: []byte("approval expired")}}, nil
		}
		return &domain.Handoff{Payload: &domain.Payload{Type: "text", Data: []byte("INV-1 open 1 200 EUR\nINV-2 open 300 EUR")}}, nil
	}}
	first := bootSubtaskKernel(t, dsn, cfg, agentA).runSubtasks(t, e2eBrief("acme"), e2eBrief("zeta"))
	if first[0].Status != subtasks.StatusDone || first[1].Status != subtasks.StatusFailed || agentA.count() != 2 {
		t.Fatalf("kernel A: %+v (dispatches %d)", first, agentA.count())
	}

	// The kernel restarts: a fresh stack on the same database. The parent resumes
	// by retrying the identical call.
	agentB := &subtaskAgent{answer: func(_ context.Context, h *domain.Handoff) (*domain.Handoff, error) {
		return &domain.Handoff{Payload: &domain.Payload{Type: "text", Data: []byte("no open invoices for zeta")}}, nil
	}}
	again := bootSubtaskKernel(t, dsn, cfg, agentB).runSubtasks(t, e2eBrief("acme"), e2eBrief("zeta"))
	if !again[0].Resumed || again[0].Status != subtasks.StatusDone || again[0].SubtaskID != first[0].SubtaskID ||
		!strings.Contains(again[0].Summary, "INV-2 open 300 EUR") {
		t.Fatalf("the finished subtask must be answered from its durable result: %+v", again[0])
	}
	if agentB.count() != 1 || !strings.Contains(string(agentB.handoffs[0].Payload.Data), "zeta") ||
		again[1].Status != subtasks.StatusDone || again[1].Resumed {
		t.Fatalf("only the unfinished subtask runs again after the restart (dispatches %d): %+v", agentB.count(), again[1])
	}
	// The record is the ADR-0154 action record, keyed deterministically.
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var state, tool string
	if err := pool.QueryRow(context.Background(), `SELECT state, tool FROM action_records WHERE key = $1`,
		domain.SubtaskResultKey(first[0].SubtaskID)).Scan(&state, &tool); err != nil || state != "accepted" || tool != subtasks.ToolName {
		t.Fatalf("durable record: state=%q tool=%q err=%v", state, tool, err)
	}
}

func TestPg_RunSubtasks_WallTimeCapForcesASummary(t *testing.T) {
	dsn, _ := pgscratch.Migrated(t)
	cleanSubtaskRecords(t, dsn)
	t.Cleanup(func() { cleanSubtaskRecords(t, dsn) })
	cfg := singleAgentComposed()
	cfg.Lane.Subtasks.WorkerMaxWallSeconds = 1
	cfg.Lane.Subtasks.WorkerSummaryGraceSeconds = 1

	// The agent honours the stamped stopping rule as the SDK does: at its wall
	// cap it answers with one forced summary, partial, exit_reason wall.
	agent := &subtaskAgent{answer: func(ctx context.Context, h *domain.Handoff) (*domain.Handoff, error) {
		if h.Context[domain.HandoffKeyLane] != domain.LaneSingleAgent || h.Context[domain.HandoffKeyLaneMaxWallSeconds] != "1" {
			return &domain.Handoff{Payload: &domain.Payload{Type: "error", Data: []byte("no wall cap stamped")}}, nil
		}
		if strings.Contains(string(h.Payload.Data), "hung") {
			// A worker that never answers: the kernel ends it at cap + grace.
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return &domain.Handoff{Payload: &domain.Payload{Type: "text", Data: []byte("Found INV-1; ran out of time before the rest."),
			Metadata: map[string]string{domain.AgentResultPartial: "true", domain.AgentResultExitReason: "wall"}}}, nil
	}}
	k := bootSubtaskKernel(t, dsn, cfg, agent)
	start := time.Now()
	res := k.runSubtasks(t, e2eBrief("capped"), e2eBrief("hung"))
	if res[0].Status != subtasks.StatusPartial || res[0].ExitReason != subtasks.ExitWall || !strings.Contains(res[0].Summary, "INV-1") {
		t.Fatalf("the worker's forced summary: %+v", res[0])
	}
	if res[1].Status != subtasks.StatusPartial || res[1].ExitReason != subtasks.ExitWall || res[1].Summary == "" {
		t.Fatalf("a hung worker still returns a kernel-written summary: %+v", res[1])
	}
	if el := time.Since(start); el < 2*time.Second || el > 30*time.Second {
		t.Fatalf("the kernel ends a hung worker at wall + grace (2 s): %v", el)
	}
	// A partial subtask is not kept: the retry runs it again.
	before := agent.count()
	k.runSubtasks(t, e2eBrief("capped"))
	if agent.count() != before+1 {
		t.Fatal("a partial (capped) subtask must run again on retry")
	}
}
