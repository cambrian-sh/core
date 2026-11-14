package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/mcpserve"
	session "github.com/cambrian-sh/core/internal/substrate/session"
)

// memSessions is a minimal in-memory session.SessionRepository for the lane
// tests — the same shape the network package's tests use.
type memSessions struct {
	mu sync.Mutex
	m  map[domain.SessionID]domain.Session
}

func newMemSessions() *memSessions { return &memSessions{m: map[domain.SessionID]domain.Session{}} }

func (s *memSessions) SaveSession(_ context.Context, ses domain.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[ses.ID] = ses
	return nil
}

func (s *memSessions) GetSession(_ context.Context, id domain.SessionID) (*domain.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ses, ok := s.m[id]; ok {
		cp := ses
		return &cp, nil
	}
	return nil, nil
}

func (s *memSessions) ListSessions(_ context.Context, status domain.SessionStatus) ([]domain.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.Session
	for _, ses := range s.m {
		if status == "" || ses.Status == status {
			out = append(out, ses)
		}
	}
	return out, nil
}

// waitTerminal polls the lane until the task leaves "running" or the deadline
// passes — the same contract an external caller has.
func waitTerminal(t *testing.T, lane *mcpTaskLane, id string) mcpserve.TaskView {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if v, ok := lane.Status(context.Background(), id); ok && mcpserve.TaskTerminal(v.State) {
			return v
		}
		time.Sleep(5 * time.Millisecond)
	}
	v, _ := lane.Status(context.Background(), id)
	t.Fatalf("task %s never reached a terminal state (still %q)", id, v.State)
	return mcpserve.TaskView{}
}

// Submit opens a real session (the task IS a session — the operator lane's own
// aggregate), dispatches through the execute closure with the session
// presented, and the successful outcome carries the execution's result.
func TestMCPTaskLane_SubmitRunsTheExistingLane(t *testing.T) {
	repo := newMemSessions()
	mgr := session.New(repo)
	var gotSession, gotTask string
	lane := newMCPTaskLane(mgr, nil, func(_ context.Context, sessionID, task string) (string, map[string]string, error) {
		gotSession, gotTask = sessionID, task
		return "the produced answer", nil, nil
	})

	id, err := lane.Submit(context.Background(), "inventory the repo", "", domain.AgentPrincipal("mcp:alice"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	ses, err := repo.GetSession(context.Background(), domain.SessionID(id))
	if err != nil || ses == nil {
		t.Fatalf("no session was opened for the task: %v", err)
	}
	if ses.Goal != "inventory the repo" || ses.Status != domain.SessionActive {
		t.Fatalf("session = %+v, want an active session carrying the task as its goal", ses)
	}

	v := waitTerminal(t, lane, id)
	if v.State != mcpserve.TaskStateSucceeded || v.Result != "the produced answer" {
		t.Fatalf("terminal view = %+v, want succeeded with the result", v)
	}
	if gotSession != id || gotTask != "inventory the repo" {
		t.Fatalf("dispatch got (%q, %q), want the task under its own session", gotSession, gotTask)
	}
	if lane.BeneficiaryOf(domain.SessionID(id)) != domain.AgentPrincipal("mcp:alice") {
		t.Fatalf("BeneficiaryOf = %+v, want mcp:alice", lane.BeneficiaryOf(domain.SessionID(id)))
	}
}

// A dispatch error is a terminal FAILED state whose note is classified, not
// the raw error string — the note reaches an external caller.
func TestMCPTaskLane_DispatchErrorIsClassifiedFailure(t *testing.T) {
	lane := newMCPTaskLane(session.New(newMemSessions()), nil,
		func(context.Context, string, string) (string, map[string]string, error) {
			return "", nil, errors.New("rpc error: dial tcp 127.0.0.1:11434: connection refused")
		})
	id, err := lane.Submit(context.Background(), "x", "", domain.AgentPrincipal("mcp:alice"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	v := waitTerminal(t, lane, id)
	if v.State != mcpserve.TaskStateFailed {
		t.Fatalf("state = %q, want failed", v.State)
	}
	if !strings.HasPrefix(v.Result, taskFailurePrefix) {
		t.Fatalf("result %q does not carry the failure prefix", v.Result)
	}
	if strings.Contains(v.Result, "127.0.0.1") || strings.Contains(v.Result, "dial tcp") {
		t.Fatalf("result %q leaks the raw transport error to an external caller", v.Result)
	}
}

// Execute answers a partial plan as a PAYLOAD (the operator watches the feed);
// for a polling caller it is a failure with the narrative as detail.
func TestMCPTaskLane_PartialPlanIsFailure(t *testing.T) {
	lane := newMCPTaskLane(session.New(newMemSessions()), nil,
		func(context.Context, string, string) (string, map[string]string, error) {
			return "step 2 (apply the migration) failed", map[string]string{"_partial_plan": "true"}, nil
		})
	id, err := lane.Submit(context.Background(), "x", "", domain.AgentPrincipal("mcp:alice"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	v := waitTerminal(t, lane, id)
	if v.State != mcpserve.TaskStateFailed || !strings.Contains(v.Result, "step 2") {
		t.Fatalf("view = %+v, want failed carrying the partial-plan narrative", v)
	}
}

// Operator steering on the underlying session shows through while the task
// runs: a paused session polls as paused.
func TestMCPTaskLane_StatusReflectsOperatorSteering(t *testing.T) {
	mgr := session.New(newMemSessions())
	release := make(chan struct{})
	lane := newMCPTaskLane(mgr, nil, func(context.Context, string, string) (string, map[string]string, error) {
		<-release
		return "done", nil, nil
	})
	id, err := lane.Submit(context.Background(), "long task", "", domain.AgentPrincipal("mcp:alice"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := mgr.TransitionStatusReason(context.Background(), domain.SessionID(id), domain.SessionPaused, "operator paused it"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if v, ok := lane.Status(context.Background(), id); !ok || v.State != mcpserve.TaskStatePaused {
		t.Fatalf("paused task polls as %+v, want paused", v)
	}
	if err := mgr.TransitionStatusReason(context.Background(), domain.SessionID(id), domain.SessionActive, "resumed"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	close(release)
	if v := waitTerminal(t, lane, id); v.State != mcpserve.TaskStateSucceeded {
		t.Fatalf("resumed task ended %q, want succeeded", v.State)
	}
}

// Defense in depth under the transport check: the lane itself refuses a zero
// beneficiary — a task nobody is the beneficiary of must not exist.
func TestMCPTaskLane_RefusesZeroBeneficiary(t *testing.T) {
	lane := newMCPTaskLane(session.New(newMemSessions()), nil,
		func(context.Context, string, string) (string, map[string]string, error) { return "", nil, nil })
	if _, err := lane.Submit(context.Background(), "x", "", domain.PrincipalRef{}); err == nil {
		t.Fatal("a zero-beneficiary submission was accepted")
	}
}

// An unknown task resolves to a ZERO beneficiary — fail-closed for every
// session this lane did not submit.
func TestMCPTaskLane_BeneficiaryOfUnknownTaskIsZero(t *testing.T) {
	lane := newMCPTaskLane(session.New(newMemSessions()), nil,
		func(context.Context, string, string) (string, map[string]string, error) { return "", nil, nil })
	if got := lane.BeneficiaryOf("never-submitted"); !got.IsZero() {
		t.Fatalf("BeneficiaryOf(unknown) = %+v, want zero", got)
	}
}

// T-20261103-per-plan-maps-in-memory-agent-stepdocs: the task index is bounded.
// A FINISHED task older than the retention window is dropped on the next
// submission; a task still running is kept however old it is (BeneficiaryOf
// must keep answering for it), and a recent finished task stays pollable.
func TestMCPTaskLane_PrunesFinishedTasksPastRetention(t *testing.T) {
	release := make(chan struct{})
	lane := newMCPTaskLane(session.New(newMemSessions()), nil,
		func(_ context.Context, _, task string) (string, map[string]string, error) {
			if task == "slow" {
				<-release
			}
			return "done", nil, nil
		})
	defer close(release)
	submit := func(task string) string {
		t.Helper()
		id, err := lane.Submit(context.Background(), task, "", domain.AgentPrincipal("mcp:alice"))
		if err != nil {
			t.Fatalf("submit %s: %v", task, err)
		}
		return id
	}
	old := submit("old")
	waitTerminal(t, lane, old)
	recent := submit("recent")
	waitTerminal(t, lane, recent)
	slow := submit("slow")

	stale := time.Now().UTC().Add(-mcpTaskRetention - time.Hour)
	lane.mu.Lock()
	lane.tasks[domain.SessionID(old)].updatedAt = stale
	lane.tasks[domain.SessionID(slow)].updatedAt = stale
	lane.mu.Unlock()

	submit("next")
	if _, ok := lane.Status(context.Background(), old); ok {
		t.Errorf("finished task past retention is still indexed")
	}
	if _, ok := lane.Status(context.Background(), recent); !ok {
		t.Errorf("recent finished task was pruned")
	}
	if lane.BeneficiaryOf(domain.SessionID(slow)).IsZero() {
		t.Errorf("a running task was pruned (its beneficiary is gone)")
	}
}

// ENG-1467 (owner decision 2026-11-14): the task runs on the authenticated MCP
// caller's context, detached from its cancellation — the principal and surface
// reach Execute, and the submitting request ending does not cancel the run.
func TestMCPTaskLane_ExecuteCarriesCallerPrincipalDetached(t *testing.T) {
	type seen struct {
		principal domain.PrincipalRef
		surface   domain.SurfaceRef
		ctxErr    error
	}
	got := make(chan seen, 1)
	release := make(chan struct{})
	lane := newMCPTaskLane(session.New(newMemSessions()), nil,
		func(ctx context.Context, _, _ string) (string, map[string]string, error) {
			<-release // the submitting request is cancelled before the run proceeds
			got <- seen{domain.PrincipalFromContext(ctx), domain.SurfaceFromContext(ctx), ctx.Err()}
			return "ok", nil, nil
		})

	caller := domain.AgentPrincipal("mcp:alice")
	reqCtx, cancel := context.WithCancel(context.Background())
	reqCtx = domain.WithPrincipal(reqCtx, caller)
	reqCtx = domain.WithSurface(reqCtx, domain.SurfaceRef{Kind: domain.SurfaceMCP})
	id, err := lane.Submit(reqCtx, "do the thing", "", caller)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	cancel()
	close(release)

	s := <-got
	if s.principal != caller {
		t.Fatalf("Execute principal = %+v, want the MCP caller %+v", s.principal, caller)
	}
	if s.surface.Kind != domain.SurfaceMCP {
		t.Fatalf("Execute surface = %+v, want mcp", s.surface)
	}
	if s.ctxErr != nil {
		t.Fatalf("Execute ctx err = %v: the request's cancellation reached the task", s.ctxErr)
	}
	if v := waitTerminal(t, lane, id); v.State != mcpserve.TaskStateSucceeded {
		t.Fatalf("terminal view = %+v, want succeeded", v)
	}
}

// ENG-1467 / AGENTS.md §3.3: the lane owns its runs — Stop cancels an in-flight
// run (whose submit request is long gone), waits for it, and refuses new work.
func TestMCPTaskLane_StopCancelsAndDrainsRuns(t *testing.T) {
	started := make(chan struct{})
	lane := newMCPTaskLane(session.New(newMemSessions()), nil,
		func(ctx context.Context, _, _ string) (string, map[string]string, error) {
			close(started)
			<-ctx.Done() // a long run: only Stop can end it
			return "", nil, ctx.Err()
		})
	caller := domain.AgentPrincipal("mcp:alice")
	reqCtx, cancelReq := context.WithCancel(context.Background())
	id, err := lane.Submit(reqCtx, "long task", "", caller)
	if err != nil {
		t.Fatal(err)
	}
	cancelReq() // the request ending must not end the run
	<-started

	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lane.Stop(stopCtx)
	if stopCtx.Err() != nil {
		t.Fatal("Stop did not drain the run within its budget")
	}
	if v, _ := lane.Status(context.Background(), id); v.State != mcpserve.TaskStateFailed {
		t.Fatalf("stopped run state = %q, want failed", v.State)
	}
	if _, err := lane.Submit(context.Background(), "late", "", caller); err == nil {
		t.Fatal("a stopped lane accepted a new task")
	}
}
