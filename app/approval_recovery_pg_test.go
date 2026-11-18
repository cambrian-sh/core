package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/metadata"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
	"github.com/cambrian-sh/core/internal/testing/pgscratch"
)

// T-20261118-a-sync-approval-orphaned-by-a-kernel-restart, end to end on
// Postgres through the production path: the hub, its stores and the action
// record built from config (buildApprovalHub), the executor, the schedule gate
// (buildScheduledTasks), the recoverers (wireApprovalRecovery), the principal
// registry (buildPrincipalRegistry), and the agent-plane RPCs that raise the
// approvals (Server.ExecuteTool, Server.ProposeScheduledTask). A kernel run is
// abandoned the way a killed process is — its waiters never resolve anything —
// and a fresh stack boots on the same database. Stand-ins: the tool process,
// the premium schedule store (shared, as its table is) and the lease book.

type recoveryTool struct{ n atomic.Int32 }

func (h *recoveryTool) Execute(_ context.Context, call domain.ToolCall) ([]byte, error) {
	n := h.n.Add(1)
	return []byte(fmt.Sprintf(`{"issue":"OPS-%d","args":%s}`, n, call.ArgsJSON)), nil
}

// sharedSchedules stands in for the premium schedule store: its rows outlive a
// kernel run, as the table does.
type sharedSchedules struct {
	mu    sync.Mutex
	tasks map[string]domain.ScheduledTask
}

func (s *sharedSchedules) Check(context.Context, domain.ScheduledTaskSpec, domain.ScheduleLimits) error {
	return nil
}
func (s *sharedSchedules) Create(_ context.Context, sp domain.ScheduledTaskSpec, _ domain.ScheduleLimits) (domain.ScheduledTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := domain.ScheduledTaskID(sp)
	t := domain.ScheduledTask{ID: id, OwnerID: sp.OwnerID, Title: sp.Title, Goal: sp.Goal, Cron: sp.Cron,
		Delivery: sp.Delivery, Active: true, ApprovedBy: sp.ApprovedBy}
	s.tasks[id] = t
	return t, nil
}
func (s *sharedSchedules) List(_ context.Context, owner string) ([]domain.ScheduledTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.ScheduledTask
	for _, t := range s.tasks {
		if t.OwnerID == owner {
			out = append(out, t)
		}
	}
	return out, nil
}
func (s *sharedSchedules) SetActive(context.Context, string, string, bool) error { return nil }
func (s *sharedSchedules) Delete(context.Context, string, string) error          { return nil }

type recoveryKernel struct {
	hub    *domain.AddressedHub
	server *subnetwork.Server
	tool   *recoveryTool
	evs    <-chan domain.ApprovalEvent
}

const recoveryAgent = "assistant"

// bootRecoveryKernel is one kernel process over the database: a fresh pool,
// hub, registry, executor, schedule gate and server — nothing shared with an
// earlier boot but Postgres and the schedule store.
func bootRecoveryKernel(t *testing.T, dsn, person, lease, conv string, schedules *sharedSchedules) *recoveryKernel {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	cfg := config.DefaultConfig()
	hub, err := buildApprovalHub(ctx, cfg, pool)
	if err != nil {
		t.Fatal(err)
	}
	ids := buildPrincipalRegistry(ctx, pool)
	if _, _, err := ids.registry.EnsurePrincipal(ctx, domain.Principal{ID: person, Kind: domain.PrincipalUser, Source: domain.SourceLocal}); err != nil {
		t.Fatal(err)
	}
	reg := domain.NewInMemoryToolRegistry()
	reg.Register(domain.SystemTool{Name: "create_issue", Dangerous: true, Effects: []domain.ToolEffect{domain.EffectWrite}})
	grants := domain.NewInMemoryGrantsStore()
	grants.Set(recoveryAgent, []domain.ToolGrant{{Tool: "create_issue", Policy: domain.ToolResourcePolicy{AllowAll: true}}})
	tool := &recoveryTool{}
	te := newTestToolExecutor(t, domain.ToolExecutorControls{Registry: reg, Grants: grants, Approval: hub},
		domain.ToolExecutor{Handler: tool, InlineThreshold: 1 << 20})
	sched := buildScheduledTasks(cfg.Execution.Schedules, schedules, hub)
	if sched == nil {
		t.Fatal("scheduled tasks not built from the default config")
	}
	forPerson := func(context.Context, domain.LeaseBinding) string { return person }
	srv := &subnetwork.Server{
		ToolExecutor: te, ScheduledTasks: sched, ApprovalHub: hub,
		LLMGateway:   &subtaskE2EGateway{bindings: map[domain.LeaseID]domain.LeaseBinding{domain.LeaseID(lease): {AgentID: recoveryAgent, ConversationID: conv}}},
		RunPrincipal: forPerson, ConversationOwner: forPerson,
	}
	wireApprovalRecovery(hub, te, sched, func() domain.PrincipalDirectory { return principalDirectoryOrRegistry(nil, ids.registry) })
	evs, stopWatch := hub.Watch(domain.ApprovalWatch{For: domain.OperatorApprover("op"), All: true, Resolutions: true})
	hub.Start(ctx)
	t.Cleanup(func() { stopWatch(); hub.Stop() })
	return &recoveryKernel{hub: hub, server: srv, tool: tool, evs: evs}
}

func agentCtx(ctx context.Context) context.Context {
	return metadata.NewIncomingContext(ctx, metadata.Pairs("x-agent-id", recoveryAgent))
}

func (k *recoveryKernel) nextEvent(t *testing.T, typ domain.ApprovalEventType, id string) domain.ApprovalItem {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-k.evs:
			if ev.Type == typ && (id == "" || ev.Item.ID == id) {
				return ev.Item
			}
		case <-deadline:
			t.Fatalf("no %s event for %q", typ, id)
		}
	}
}

func TestE2E_ApprovalApprovedAfterRestartRunsOnce_Pg(t *testing.T) {
	dsn, _ := pgscratch.Migrated(t)
	suffix := fmt.Sprint(time.Now().UnixNano())
	person, lease, conv := "eng2118-lena-"+suffix, "lease-eng2118-"+suffix, "conv-eng2118-"+suffix
	schedules := &sharedSchedules{tasks: map[string]domain.ScheduledTask{}}

	// ── kernel run 1: both approvals raised; the process "dies" while they wait.
	k1 := bootRecoveryKernel(t, dsn, person, lease, conv, schedules)
	run1, kill := context.WithCancel(agentCtx(context.Background()))
	defer kill() // only at the end: by then kernel 2 took the decisions (see takeAfterResolve)
	toolDone := make(chan *pb.ExecuteToolResponse, 1)
	go func() {
		resp, _ := k1.server.ExecuteTool(run1, &pb.ExecuteToolRequest{ToolName: "create_issue", LeaseId: lease,
			ArgsJson: `{"project":"OPS","summary":"Database outage ` + suffix + `"}`})
		toolDone <- resp
	}()
	toolItem := k1.nextEvent(t, domain.ApprovalEventRaised, "")
	schedDone := make(chan *pb.ProposeScheduledTaskResponse, 1)
	go func() {
		resp, _ := k1.server.ProposeScheduledTask(run1, &pb.ProposeScheduledTaskRequest{LeaseId: lease,
			Title: "weekly " + suffix, Goal: "Summarise my week", Cron: "0 9 * * 1"})
		schedDone <- resp
	}()
	schedItem := k1.nextEvent(t, domain.ApprovalEventRaised, "")
	if toolItem.Intent == nil || schedItem.Intent == nil || schedItem.Kind != domain.ApprovalKindSchedule {
		t.Fatalf("raised items carry no intent: tool=%+v sched=%+v", toolItem.Intent, schedItem.Intent)
	}

	// ── kernel run 2 on the same database.
	k2 := bootRecoveryKernel(t, dsn, person, lease, conv, schedules)
	pending, err := k2.hub.List(context.Background(), domain.ApprovalQuery{States: []domain.ApprovalState{domain.ApprovalPending}})
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, it := range pending {
		listed[it.ID] = true
	}
	if !listed[toolItem.ID] || !listed[schedItem.ID] {
		t.Fatalf("the restarted kernel does not list both pending approvals: %v", listed)
	}

	// The dangerous tool call: approved by the operator its rule names.
	res, err := k2.hub.Answer(context.Background(), toolItem.ID, domain.ApprovalAnswer{Approved: true, By: domain.OperatorApprover("op")})
	if err != nil || res.State != domain.ApprovalApproved || !strings.Contains(res.Detail, "create_issue ran") {
		t.Fatalf("tool approve after restart: %+v %v", res, err)
	}
	if _, err := k2.hub.Answer(context.Background(), toolItem.ID, domain.ApprovalAnswer{Approved: true, By: domain.OperatorApprover("op")}); !errors.Is(err, domain.ErrApprovalResolved) {
		t.Fatalf("second tool approve: %v", err)
	}
	if k2.tool.n.Load() != 1 || k1.tool.n.Load() != 0 {
		t.Fatalf("tool ran %d times on kernel 2 and %d on kernel 1, want 1 and 0", k2.tool.n.Load(), k1.tool.n.Load())
	}
	// The outcome reaches the approval feed after the approval itself.
	k2.nextEvent(t, domain.ApprovalEventResolved, toolItem.ID)
	if out := k2.nextEvent(t, domain.ApprovalEventResolved, toolItem.ID); !strings.Contains(out.Detail, "approved and carried out") {
		t.Fatalf("feed outcome: %+v", out)
	}

	// The schedule: approved by the person it is for.
	me := domain.Approver{Principal: domain.PersonPrincipal(person)}
	res, err = k2.hub.Answer(context.Background(), schedItem.ID, domain.ApprovalAnswer{Approved: true, By: me})
	if err != nil || !strings.Contains(res.Detail, "created scheduled task") {
		t.Fatalf("schedule approve after restart: %+v %v", res, err)
	}
	if _, err := k2.hub.Answer(context.Background(), schedItem.ID, domain.ApprovalAnswer{Approved: true, By: me}); !errors.Is(err, domain.ErrApprovalResolved) {
		t.Fatalf("second schedule approve: %v", err)
	}
	tasks, _ := schedules.List(context.Background(), person)
	if len(tasks) != 1 || tasks[0].ApprovedBy == "" || tasks[0].Goal != "Summarise my week" {
		t.Fatalf("tasks = %+v", tasks)
	}

	// What the person reads back: both items resolved, the outcome stated, no
	// intent left behind; the ADR-0154 records settled.
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	actions := buildActionRecordStore(context.Background(), pool)
	for _, id := range []string{toolItem.ID, schedItem.ID} {
		got, err := k2.hub.Get(context.Background(), id)
		if err != nil || got.State != domain.ApprovalApproved || got.Intent != nil || !got.Consumed || !strings.Contains(got.Detail, "carried out") {
			t.Fatalf("stored %s: %+v %v", id, got, err)
		}
		if ar, err := actions.Get(context.Background(), domain.RecoveryActionKey(id)); err != nil || ar.State != domain.ActionStateAccepted {
			t.Fatalf("action record of %s: %+v %v", id, ar, err)
		}
	}

	// Kernel run 1's requesters, finally cut loose, do nothing.
	kill()
	if r := <-toolDone; r != nil && !r.Denied {
		t.Fatalf("kernel 1's abandoned call reported a run: %+v", r)
	}
	<-schedDone
	if k1.tool.n.Load() != 0 {
		t.Fatal("kernel 1 ran the call too")
	}
	if tasks, _ := schedules.List(context.Background(), person); len(tasks) != 1 {
		t.Fatalf("kernel 1 created the task too: %+v", tasks)
	}

	// A third kernel run starts with nothing left to recover.
	k3 := bootRecoveryKernel(t, dsn, person, lease, conv, schedules)
	if k3.tool.n.Load() != 0 {
		t.Fatal("a later boot re-ran a recovered action")
	}
}

// ENG-2119 compatibility: an approval withdrawn because its requester stopped
// waiting is final. Approved on a restarted kernel, it is refused and runs
// nothing.
func TestE2E_WithdrawnApprovalApprovedAfterRestartIsRefused_Pg(t *testing.T) {
	dsn, _ := pgscratch.Migrated(t)
	suffix := fmt.Sprint(time.Now().UnixNano())
	person, lease, conv := "eng2118-mira-"+suffix, "lease-eng2118w-"+suffix, "conv-eng2118w-"+suffix
	schedules := &sharedSchedules{tasks: map[string]domain.ScheduledTask{}}

	k1 := bootRecoveryKernel(t, dsn, person, lease, conv, schedules)
	turn, endTurn := context.WithCancel(agentCtx(context.Background()))
	done := make(chan *pb.ExecuteToolResponse, 1)
	go func() {
		resp, _ := k1.server.ExecuteTool(turn, &pb.ExecuteToolRequest{ToolName: "create_issue", LeaseId: lease,
			ArgsJson: `{"project":"OPS","summary":"withdrawn ` + suffix + `"}`})
		done <- resp
	}()
	it := k1.nextEvent(t, domain.ApprovalEventRaised, "")
	endTurn() // the turn ends while the approval is pending: withdrawn
	if r := <-done; r == nil || !r.Denied {
		t.Fatalf("the ended call: %+v", r)
	}
	if got, _ := k1.hub.Get(context.Background(), it.ID); got.State != domain.ApprovalWithdrawn {
		t.Fatalf("state after the turn ended = %s", got.State)
	}

	k2 := bootRecoveryKernel(t, dsn, person, lease, conv, schedules)
	res, err := k2.hub.Answer(context.Background(), it.ID, domain.ApprovalAnswer{Approved: true, By: domain.OperatorApprover("op")})
	if !errors.Is(err, domain.ErrApprovalResolved) || res.State != domain.ApprovalWithdrawn || !strings.Contains(err.Error(), "nothing was done") {
		t.Fatalf("withdrawn approval approved after restart: %+v %v", res, err)
	}
	if k1.tool.n.Load()+k2.tool.n.Load() != 0 {
		t.Fatal("a withdrawn approval ran its call")
	}
}

// An approve for a person who left while the kernel was down is refused with
// the reason, and the item is withdrawn.
func TestE2E_ApprovalForDeprovisionedPersonRefusedAfterRestart_Pg(t *testing.T) {
	dsn, _ := pgscratch.Migrated(t)
	suffix := fmt.Sprint(time.Now().UnixNano())
	person, lease, conv := "eng2118-omar-"+suffix, "lease-eng2118d-"+suffix, "conv-eng2118d-"+suffix
	schedules := &sharedSchedules{tasks: map[string]domain.ScheduledTask{}}

	k1 := bootRecoveryKernel(t, dsn, person, lease, conv, schedules)
	run1, kill := context.WithCancel(agentCtx(context.Background()))
	defer kill()
	go func() {
		_, _ = k1.server.ProposeScheduledTask(run1, &pb.ProposeScheduledTaskRequest{LeaseId: lease,
			Title: "daily " + suffix, Goal: "Brief me", Cron: "0 8 * * *"})
	}()
	it := k1.nextEvent(t, domain.ApprovalEventRaised, "")

	k2 := bootRecoveryKernel(t, dsn, person, lease, conv, schedules)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := buildPrincipalRegistry(context.Background(), pool).registry.SetStatus(context.Background(), person, domain.PrincipalDeprovisioned); err != nil {
		t.Fatal(err)
	}
	res, err := k2.hub.Answer(context.Background(), it.ID, domain.ApprovalAnswer{Approved: true, By: domain.OperatorApprover("op")})
	if !errors.Is(err, domain.ErrApprovalNotCarriedOut) || res.State != domain.ApprovalWithdrawn ||
		!strings.Contains(err.Error(), "no longer a known, active person") {
		t.Fatalf("approve for a deprovisioned person: %+v %v", res, err)
	}
	if tasks, _ := schedules.List(context.Background(), person); len(tasks) != 0 {
		t.Fatalf("a task was created for a person who left: %+v", tasks)
	}
}
