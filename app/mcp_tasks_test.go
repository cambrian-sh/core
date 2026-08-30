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
