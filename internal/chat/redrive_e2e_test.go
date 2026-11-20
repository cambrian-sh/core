package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	// dead: killed, but still registered (the crash watcher has not seen the exit).
	dead bool
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
	// linger > 0 models the real crash watcher (ENG-2266): a killed worker stays
	// registered — "live" — for linger, its socket refusing dials; then it is
	// deregistered, and restartAfter later the restart policy boots a replacement.
	linger, restartAfter time.Duration
	// killAll: a kill takes down every worker of the pool (the chaos harness kills
	// every chat worker), each lingering and being replaced as above.
	killAll bool
	dials   int // dispatches that hit a dead worker's socket
	// quarantine: the restart policy quarantines a killed stream as a crash-loop
	// instead of booting a replacement (ENG-2269), as AgentManager.scheduleRestart
	// does past the flap limit.
	quarantine  bool
	quarantined map[string]bool
	wg          sync.WaitGroup
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
	if w != nil && w.dead {
		d.dials++
		d.mu.Unlock()
		return nil, status.Error(codes.Unavailable, `connection error: desc = "transport: Error while dialing: dial unix sim.sock: connect: No connection could be made because the target machine actively refused it."`)
	}
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
		// As AgentManager.CallDaemon refuses a stream with no live instance.
		return nil, fmt.Errorf("CallDaemon: %w: no daemon registered for stream %q", domain.ErrDaemonUnavailable, stream)
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
		// SIGKILL: the process is gone.
		d.mu.Lock()
		if d.linger > 0 {
			// Still registered until the crash watcher notices; then gone; then
			// replaced by the restart policy.
			victims := map[string]*simWorker{stream: w}
			if d.killAll {
				for st, other := range d.byStr {
					victims[st] = other
				}
			}
			for st, dead := range victims {
				dead.dead = true
				d.wg.Add(1)
				go d.crashAndRestart(st, dead)
			}
		} else {
			delete(d.byStr, stream)
		}
		d.mu.Unlock()
		return nil, status.Error(codes.Unavailable, "error reading from server: connection reset by peer")
	}
	return &domain.Handoff{Payload: &domain.Payload{Data: []byte("answered by " + w.instance)}}, nil
}

// crashAndRestart is the crash watcher and the restart policy for one killed worker.
func (d *simDaemons) crashAndRestart(stream string, dead *simWorker) {
	defer d.wg.Done()
	time.Sleep(d.linger)
	d.mu.Lock()
	if d.byStr[stream] == dead {
		delete(d.byStr, stream)
	}
	d.mu.Unlock()
	time.Sleep(d.restartAfter)
	d.mu.Lock()
	if d.quarantine {
		if d.quarantined == nil {
			d.quarantined = map[string]bool{}
		}
		d.quarantined[stream] = true
		d.mu.Unlock()
		return
	}
	if _, taken := d.byStr[stream]; !taken {
		d.boots++
		id := fmt.Sprintf("inst-%d", d.boots)
		d.byStr[stream] = &simWorker{instance: id}
		d.spawned = append(d.spawned, id)
	}
	d.mu.Unlock()
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

func (d *simDaemons) DaemonQuarantined(stream string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.quarantined[stream]
}

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
	return redriveLaneWith(t, 0, 0, plans...)
}

func redriveLaneWith(t *testing.T, linger, restartAfter time.Duration, plans ...simPlan) (*TurnService, *simDaemons, *sendHandler) {
	t.Helper()
	return redriveLaneSized(t, 1, false, linger, restartAfter, plans...)
}

func redriveLaneSized(t *testing.T, size int, killAll bool, linger, restartAfter time.Duration, plans ...simPlan) (*TurnService, *simDaemons, *sendHandler) {
	t.Helper()
	reg := domain.NewInMemoryToolRegistry()
	reg.Register(domain.SystemTool{Name: "mail_send", Effects: []domain.ToolEffect{domain.EffectWrite}})
	handler := &sendHandler{}
	turnCalls := domain.NewTurnCalls(domain.NewDurableIdempotencyCache(time.Hour, domain.NewMemActionRecordStore()))
	exec := newTestToolExecutor(t, domain.ToolExecutorControls{Registry: reg, Unrestricted: true, TurnCalls: turnCalls},
		domain.ToolExecutor{Handler: handler, InlineThreshold: 1 << 20})
	daemons := &simDaemons{byStr: map[string]*simWorker{}, exec: exec, plans: plans, linger: linger, restartAfter: restartAfter, killAll: killAll}
	t.Cleanup(daemons.wg.Wait)
	pool := agentpool.New(daemons, agentpool.Config{AgentID: "chat_agent", Size: size, StreamPrefix: "chat",
		RecoverPoll: 5 * time.Millisecond})
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

// ENG-2266, the live failure: the killed worker stays registered while the crash
// watcher has not seen its exit, its socket refusing dials, and its replacement boots
// later. The re-drive must wait for the replacement — a dial into the dead socket is
// "not delivered", never a second kill — and the turn answers from the new worker.
func TestE2E_Redrive_WaitsForTheReplacementWhileTheKilledWorkerLingers(t *testing.T) {
	svc, d, _ := redriveLaneWith(t, 60*time.Millisecond, 80*time.Millisecond, simPlan{kill: true})
	start := time.Now()
	msg, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "what is on my calendar?"})
	if err != nil {
		t.Fatalf("one kill with a lingering registration ended the turn: %v", err)
	}
	d.mu.Lock()
	served, calls := append([]string(nil), d.served...), d.calls
	d.mu.Unlock()
	if calls != 2 || len(served) != 2 || served[0] == served[1] {
		t.Fatalf("delivered calls %d served %v: want the killed worker then its replacement", calls, served)
	}
	if msg.Content != "answered by "+served[1] {
		t.Fatalf("reply %q, want the replacement's", msg.Content)
	}
	if time.Since(start) < 60*time.Millisecond {
		t.Fatalf("the re-drive did not wait for the killed worker to be replaced (%s)", time.Since(start))
	}
}

// A second kill — of the replacement, under the re-driven turn — still ends it.
func TestE2E_Redrive_ASecondKillAfterTheReplacementEndsTheTurn(t *testing.T) {
	svc, d, _ := redriveLaneWith(t, 30*time.Millisecond, 30*time.Millisecond, simPlan{kill: true}, simPlan{kill: true})
	_, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "what is on my calendar?"})
	var ti *domain.TurnInterruption
	if !errors.As(err, &ti) || ti.Reason != domain.TurnInterruptAssistantRestartedTwice {
		t.Fatalf("err = %v, want assistant_restarted_twice", err)
	}
	d.mu.Lock()
	calls := d.calls
	d.mu.Unlock()
	if calls != 2 {
		t.Fatalf("%d delivered calls, want 2", calls)
	}
}

// The live run's second shape (ENG-2266): a pool of two, every worker killed. The
// re-drive may be handed the OTHER worker, still being restarted — the daemon layer
// refuses that call before it reaches anything (ErrDaemonUnavailable), so it is "not
// delivered" too, and the turn is answered once a replacement serves it.
func TestE2E_Redrive_AllWorkersKilledThePoolOfTwoStillAnswers(t *testing.T) {
	svc, d, _ := redriveLaneSized(t, 2, true, 40*time.Millisecond, 60*time.Millisecond, simPlan{kill: true})
	msg, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "what is on my calendar?"})
	if err != nil {
		t.Fatalf("killing every worker once ended the turn: %v", err)
	}
	d.mu.Lock()
	calls := d.calls
	d.mu.Unlock()
	if calls != 2 || !strings.HasPrefix(msg.Content, "answered by inst-") {
		t.Fatalf("delivered calls %d, reply %q: want the killed attempt then one answer", calls, msg.Content)
	}
}

// ENG-2269, the live failure (chaos run 5): the killed worker's stream is quarantined
// as a crash-loop instead of being restarted. The turn waiting to re-drive on it must
// end at once as assistant_unavailable, not wait for a replacement that will never
// boot until the stall cut or its deadline says "stopped making progress".
func TestE2E_Redrive_AQuarantinedWorkerEndsTheTurnAtOnce(t *testing.T) {
	svc, d, _ := redriveLaneWith(t, 30*time.Millisecond, 20*time.Millisecond, simPlan{kill: true})
	d.quarantine = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err := svc.Turn(ctx, TurnRequest{ConversationID: "c1", Text: "what is on my calendar?"})
	elapsed := time.Since(start)
	var ti *domain.TurnInterruption
	if !errors.As(err, &ti) || ti.Reason != domain.TurnInterruptAssistantUnavailable {
		t.Fatalf("err = %v after %s, want assistant_unavailable", err, elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("the turn waited %s on a quarantined stream", elapsed)
	}
	if ti.Message != Message(MsgAssistantUnavailable, "en") {
		t.Fatalf("message %q", ti.Message)
	}
	d.mu.Lock()
	calls, boots := d.calls, d.boots
	d.mu.Unlock()
	if calls != 1 || boots != 1 {
		t.Fatalf("delivered calls %d boots %d: want the killed attempt only, and no replacement", calls, boots)
	}
}
