package chat

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/agentpool"
)

// End to end through the real chat lane components (ENG-1634): the real
// TurnService, the real agentpool.Pool (its self-heal respawn included) and the
// real tool executor with the turn record the composition root wires
// (ToolExecutorControls.TurnCalls → TurnService.SetTurnCalls). Only the worker
// PROCESS is simulated: a "daemon" that answers CallDaemon, may run a tool call
// on the turn's lease through the executor first, and can be killed mid-turn —
// the transport breaks (Unavailable) and the stream has no live instance until
// the pool respawns it. The external SIGKILL of a real worker process is the
// release gate's TestChaosReal_AgentSigkill.

type simWorker struct {
	instance string
	// plan is what this worker instance does with the turn it receives.
	plan simPlan
}

type simPlan struct {
	write bool // run a side-effecting tool call on the turn's lease first
	kill  bool // then die mid-turn
}

type simDaemons struct {
	mu      sync.Mutex
	byStr   map[string]*simWorker
	boots   int
	exec    *domain.ToolExecutor
	plans   []simPlan // one per CallDaemon, in order; past the end: answer
	calls   int
	served  []string // the instance that served each call
	spawned []string
}

func (d *simDaemons) SpawnDaemon(_ context.Context, _, stream string, _ map[string]any) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.boots++
	id := fmt.Sprintf("inst-%d", d.boots)
	d.byStr[stream] = &simWorker{instance: id}
	d.spawned = append(d.spawned, id)
	return id, nil
}

func (d *simDaemons) CallDaemon(ctx context.Context, stream string, h *domain.Handoff) (*domain.Handoff, error) {
	d.mu.Lock()
	w := d.byStr[stream]
	n := d.calls
	d.calls++
	plan := simPlan{}
	if n < len(d.plans) {
		plan = d.plans[n]
	}
	if w != nil {
		d.served = append(d.served, w.instance)
	}
	d.mu.Unlock()
	if w == nil {
		return nil, status.Error(codes.Unavailable, "no worker")
	}
	lease := domain.LeaseID(h.Context["_session_token_id"])
	if plan.write {
		resp := d.exec.Execute(ctx, domain.ToolCallRequest{AgentID: "chat_agent", ToolName: "mail_send",
			SessionTokenID: lease, ArgsJSON: []byte(`{"to":"team@example.com"}`)})
		if resp.Denied || resp.Error != "" {
			return nil, fmt.Errorf("sim worker: tool call refused: %+v", resp)
		}
	}
	if plan.kill {
		// SIGKILL: the process is gone; the stream has no live instance.
		d.mu.Lock()
		delete(d.byStr, stream)
		d.mu.Unlock()
		return nil, status.Error(codes.Unavailable, "error reading from server: connection reset by peer")
	}
	return &domain.Handoff{Payload: &domain.Payload{Data: []byte("answered by " + w.instance)}}, nil
}

func (d *simDaemons) StopDaemon(stream string) error {
	d.mu.Lock()
	delete(d.byStr, stream)
	d.mu.Unlock()
	return nil
}

func (d *simDaemons) DaemonInstanceID(stream string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	w, ok := d.byStr[stream]
	if !ok {
		return "", false
	}
	return w.instance, true
}

func (d *simDaemons) DaemonServing(string, time.Time) (bool, bool) { return false, false }

type sendHandler struct {
	mu sync.Mutex
	n  int
}

func (h *sendHandler) Execute(context.Context, domain.ToolCall) ([]byte, error) {
	h.mu.Lock()
	h.n++
	h.mu.Unlock()
	return []byte(`{"sent":true}`), nil
}

func (h *sendHandler) count() int { h.mu.Lock(); defer h.mu.Unlock(); return h.n }

func redriveLane(t *testing.T, plans ...simPlan) (*TurnService, *simDaemons, *sendHandler) {
	t.Helper()
	reg := domain.NewInMemoryToolRegistry()
	reg.Register(domain.SystemTool{Name: "mail_send", Effects: []domain.ToolEffect{domain.EffectWrite}})
	handler := &sendHandler{}
	turnCalls := domain.NewTurnCalls(domain.NewDurableIdempotencyCache(time.Hour, domain.NewMemActionRecordStore()))
	exec := newTestToolExecutor(t, domain.ToolExecutorControls{Registry: reg, Unrestricted: true, TurnCalls: turnCalls},
		domain.ToolExecutor{Handler: handler, InlineThreshold: 1 << 20})
	daemons := &simDaemons{byStr: map[string]*simWorker{}, exec: exec, plans: plans}
	pool := agentpool.New(daemons, agentpool.Config{AgentID: "chat_agent", Size: 1, StreamPrefix: "chat"})
	if err := pool.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Stop)

	store := newMemStore()
	if err := store.CreateConversation(context.Background(), domain.Conversation{
		ID: "c1", OwnerID: "u1", Status: domain.ConversationOpen, Profile: domain.ProfileEmployee,
	}); err != nil {
		t.Fatal(err)
	}
	n := 0
	svc := NewTurnService(store, pool, func(context.Context, int, time.Duration) (string, func(), error) {
		n++
		return fmt.Sprintf("lease-%d", n), func() {}, nil
	})
	svc.SetTurnCalls(exec.TurnCalls()) // as app/boot_tools.go wires it
	return svc, daemons, handler
}

func TestE2E_Redrive_KilledBeforeAnyWriteTheRespawnedWorkerAnswers(t *testing.T) {
	svc, d, handler := redriveLane(t, simPlan{kill: true})
	msg, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "what is on my calendar?"})
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	d.mu.Lock()
	served, spawned := append([]string(nil), d.served...), append([]string(nil), d.spawned...)
	d.mu.Unlock()
	if len(spawned) != 2 || len(served) != 2 || served[0] == served[1] {
		t.Fatalf("spawned %v served %v: want the killed worker respawned and the re-drive served by the new one", spawned, served)
	}
	if msg.Content != "answered by "+served[1] {
		t.Fatalf("reply %q, want the respawned worker's", msg.Content)
	}
	if handler.count() != 0 {
		t.Fatalf("no write was asked for, %d ran", handler.count())
	}
}

func TestE2E_Redrive_KilledAfterAWriteFailsLoudlyAndTheWriteRanOnce(t *testing.T) {
	svc, d, handler := redriveLane(t, simPlan{write: true, kill: true}, simPlan{write: true})
	_, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "send the summary to the team"})
	var ti *domain.TurnInterruption
	if !errors.As(err, &ti) || ti.Reason != domain.TurnInterruptAssistantRestartedAfterAction {
		t.Fatalf("err = %v, want assistant_restarted_after_action", err)
	}
	if handler.count() != 1 {
		t.Fatalf("the write ran %d times, want exactly once", handler.count())
	}
	d.mu.Lock()
	calls := d.calls
	d.mu.Unlock()
	if calls != 1 {
		t.Fatalf("%d dispatches, want 1 (never re-run after a write began)", calls)
	}
}

func TestE2E_Redrive_KilledTwiceEndsTheTurn(t *testing.T) {
	svc, d, _ := redriveLane(t, simPlan{kill: true}, simPlan{kill: true})
	_, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "what is on my calendar?"})
	var ti *domain.TurnInterruption
	if !errors.As(err, &ti) || ti.Reason != domain.TurnInterruptAssistantRestartedTwice {
		t.Fatalf("err = %v, want assistant_restarted_twice", err)
	}
	d.mu.Lock()
	calls := d.calls
	d.mu.Unlock()
	if calls != 2 {
		t.Fatalf("%d dispatches, want exactly 2", calls)
	}
}
