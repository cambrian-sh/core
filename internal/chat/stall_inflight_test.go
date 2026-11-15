package chat

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/internal/substrate/network"
	"github.com/cambrian-sh/core/internal/testing/fakemodels"
)

// slowModel is one long unary generation: it emits nothing until it returns.
type slowModel struct{ d time.Duration }

func (m slowModel) GenerateRequest(ctx context.Context, _ domain.GenerateRequest) (domain.GenerateResponse, error) {
	select {
	case <-time.After(m.d):
		return domain.GenerateResponse{Text: "1. ok\n2. ok", StopReason: domain.StopEndTurn}, nil
	case <-ctx.Done():
		return domain.GenerateResponse{}, ctx.Err()
	}
}

func (slowModel) StreamRequest(context.Context, domain.GenerateRequest) (<-chan domain.StreamChunk, error) {
	ch := make(chan domain.StreamChunk)
	close(ch)
	return ch, nil
}

func (slowModel) Capabilities() domain.Capabilities { return domain.Capabilities{} }

// gatewayPool is a session worker that makes ONE model call through the real LLM
// gateway on the turn's lease — the native tool path of a chat turn.
type gatewayPool struct{ gw *network.SubstrateLLMGateway }

func (p gatewayPool) Dispatch(ctx context.Context, h *domain.Handoff) (*domain.Handoff, error) {
	resp, err := p.gw.GenerateWithTools(ctx, domain.LeaseID(h.Context["_session_token_id"]), domain.UserPrompt("700 lines"))
	if err != nil {
		return nil, err
	}
	return &domain.Handoff{Payload: &domain.Payload{Data: []byte(resp.Text)}}, nil
}

// inflightService is a TurnService wired the way app.go wires it: leases from the
// real gateway, the conversation bound onto the lease, and a liveness probe that
// joins progress (frozen here — nothing reports) with the gateway's in-flight view.
func inflightService(t *testing.T, gw *network.SubstrateLLMGateway, pool Dispatcher) *TurnService {
	t.Helper()
	store := newMemStore()
	if err := store.CreateConversation(context.Background(), domain.Conversation{
		ID: "c1", OwnerID: "u1", Status: domain.ConversationOpen, Profile: domain.ProfileEmployee,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	acquire := func(ctx context.Context, limit int, ttl time.Duration) (string, func(), error) {
		id, err := gw.Acquire(ctx, domain.StepAllocation{Winner: domain.AgentDefinition{ID: "llm:m1"}}, limit, ttl)
		return string(id), func() { _, _ = gw.Complete(context.Background(), id) }, err
	}
	svc := NewTurnService(store, pool, acquire)
	svc.SetLeaseBinder(gw, "chat_agent")
	svc.StallTimeout = 200 * time.Millisecond
	svc.TurnTimeout = 30 * time.Second
	frozen := time.Now()
	svc.SetLivenessProbe(LatestActivity(func(string) time.Time { return frozen }, gw.ConversationActivity))
	return svc
}

// E14 of the 2026-11-05 E2E: a healthy turn whose single model call outlasts the
// stall window is NOT cancelled, because the call in flight on its lease is
// liveness (T-20261105-slow-model-reliability-hardcoded-20-s). The call runs 2.5 s
// against a 200 ms window — over two watchdog ticks (the tick floor is 1 s).
func TestTurn_LongModelCallInFlightIsNotAStall(t *testing.T) {
	gw := network.NewLLMGateway(config.DefaultConfig().Execution, llm.NewSpendMeter(0, nil, nil))
	gw.SetStepModels(fakemodels.New().Set("m1", slowModel{d: 2500 * time.Millisecond}))
	svc := inflightService(t, gw, gatewayPool{gw: gw})

	got, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "list 700"})
	if err != nil {
		t.Fatalf("a turn waiting on one long generation must not be killed: %v", err)
	}
	if got.Content == "" {
		t.Fatal("empty reply")
	}
}

// The true-stall detection stands: no model call in flight and no progress for the
// window still cancels the turn, with the gateway wired into the probe.
func TestTurn_SilentTurnWithNoCallInFlightIsStillCancelled(t *testing.T) {
	gw := network.NewLLMGateway(config.DefaultConfig().Execution, llm.NewSpendMeter(0, nil, nil))
	gw.SetStepModels(fakemodels.New())
	pool := &blockingPool{release: make(chan struct{})} // never calls the model
	defer close(pool.release)
	svc := inflightService(t, gw, pool)

	start := time.Now()
	_, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "hi"})
	if !errors.Is(err, ErrTurnStalled) {
		t.Fatalf("want ErrTurnStalled, got %v", err)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("stall detection should be prompt, took %s", el)
	}
}

func TestLatestActivity_TakesTheLatestAndSkipsNil(t *testing.T) {
	early, late := time.Now().Add(-time.Minute), time.Now()
	f := LatestActivity(nil, func(string) time.Time { return early }, func(string) time.Time { return late })
	if got := f("c"); !got.Equal(late) {
		t.Fatalf("want the latest source, got %s", got)
	}
	if got := LatestActivity()("c"); !got.IsZero() {
		t.Fatalf("no sources: want zero, got %s", got)
	}
}
