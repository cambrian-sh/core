package network

import (
	"context"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// gatedModel is a unary RequestGenerator that answers only when released — one
// long generation that emits nothing until it returns.
type gatedModel struct {
	started chan struct{}
	release chan struct{}
}

func (m gatedModel) GenerateRequest(ctx context.Context, _ domain.GenerateRequest) (domain.GenerateResponse, error) {
	close(m.started)
	select {
	case <-m.release:
		return domain.GenerateResponse{Text: "done", StopReason: domain.StopEndTurn}, nil
	case <-ctx.Done():
		return domain.GenerateResponse{}, ctx.Err()
	}
}

func (m gatedModel) StreamRequest(context.Context, domain.GenerateRequest) (<-chan domain.StreamChunk, error) {
	ch := make(chan domain.StreamChunk)
	close(ch)
	return ch, nil
}

func (gatedModel) Capabilities() domain.Capabilities { return domain.Capabilities{} }

// While a unary call is outstanding on a conversation's lease, the gateway reports
// the conversation as active NOW; once it returns, it reports the return time; an
// unbound conversation reports nothing. This is the gateway's half of the chat
// liveness probe (T-20261105-slow-model-reliability-hardcoded-20-s).
func TestGateway_ConversationActivityCountsAnInFlightUnaryCall(t *testing.T) {
	gw := NewLLMGateway(config.DefaultConfig().Execution)
	m := gatedModel{started: make(chan struct{}), release: make(chan struct{})}
	useFakeModel(gw, "llm:m1", m)
	lease, err := gw.Acquire(context.Background(),
		domain.StepAllocation{Winner: domain.AgentDefinition{ID: "llm:m1"}}, 4096, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	gw.BindLease(lease, domain.LeaseBinding{ConversationID: "c1", AgentID: "chat_agent"})

	done := make(chan error, 1)
	go func() {
		_, err := gw.GenerateWithTools(context.Background(), lease, domain.UserPrompt("write 700 lines"))
		done <- err
	}()
	<-m.started

	// Well after the call started, the conversation still reads as active now.
	time.Sleep(50 * time.Millisecond)
	if got := gw.ConversationActivity("c1"); time.Since(got) > 10*time.Millisecond {
		t.Fatalf("in-flight call: activity %s ago, want ≈ now", time.Since(got))
	}
	if st := gw.GetBudgetLeaseState(lease); st == nil || st.InFlightCalls != 1 {
		t.Fatalf("want 1 call in flight, got %+v", st)
	}
	if got := gw.ConversationActivity("other"); !got.IsZero() {
		t.Fatalf("unbound conversation: want zero, got %s", got)
	}

	close(m.release)
	if err := <-done; err != nil {
		t.Fatalf("GenerateWithTools: %v", err)
	}
	returned := gw.ConversationActivity("c1")
	if st := gw.GetBudgetLeaseState(lease); st == nil || st.InFlightCalls != 0 {
		t.Fatalf("want 0 calls in flight after return, got %+v", st)
	}
	time.Sleep(30 * time.Millisecond)
	if got := gw.ConversationActivity("c1"); !got.Equal(returned) || time.Since(got) < 30*time.Millisecond {
		t.Fatalf("after return: activity must stay at the return time (%s), got %s", returned, got)
	}

	// A completed lease no longer speaks for the conversation.
	if _, err := gw.Complete(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	if got := gw.ConversationActivity("c1"); !got.IsZero() {
		t.Fatalf("completed lease: want zero, got %s", got)
	}
}

// The keepalive extends a lease's expiry but never shortens the TTL it was acquired
// with: a chat turn lease (acquired for the whole turn) must survive a delegated plan
// that runs longer than the keepalive window
// (T-20261109-chat-turn-lease-is-evicted-while-its-delegated).
func TestGateway_KeepaliveNeverShortensTheAcquiredTTL(t *testing.T) {
	gw := NewLLMGateway(config.DefaultConfig().Execution)
	turnEnd := time.Now().Add(2 * time.Hour)
	long := &domain.BudgetLeaseState{ExpiresAt: turnEnd}
	gw.mu.Lock()
	gw.touchLocked(long)
	gw.mu.Unlock()
	if !long.ExpiresAt.Equal(turnEnd) {
		t.Fatalf("keepalive moved a 2h expiry to %v", long.ExpiresAt)
	}
	short := &domain.BudgetLeaseState{ExpiresAt: time.Now().Add(time.Second)}
	gw.mu.Lock()
	gw.touchLocked(short)
	gw.mu.Unlock()
	if time.Until(short.ExpiresAt) < time.Minute {
		t.Fatalf("keepalive did not extend a short expiry: %v", short.ExpiresAt)
	}
	gw.AddSession("turn", long)
	gw.EvictExpired()
	if gw.GetBudgetLeaseState("turn") == nil {
		t.Fatal("a turn lease within its acquired TTL was evicted")
	}
}

// A lease with a call in flight is not swept as leaked, whatever its TTL says.
func TestGateway_EvictExpiredSparesALeaseWithACallInFlight(t *testing.T) {
	gw := NewLLMGateway(config.DefaultConfig().Execution)
	gw.AddSession("busy", &domain.BudgetLeaseState{ExpiresAt: time.Now().Add(-time.Minute), InFlightCalls: 1})
	gw.AddSession("idle", &domain.BudgetLeaseState{ExpiresAt: time.Now().Add(-time.Minute)})
	gw.EvictExpired()
	if gw.GetBudgetLeaseState("busy") == nil {
		t.Fatal("a lease with a call in flight was evicted")
	}
	if gw.GetBudgetLeaseState("idle") != nil {
		t.Fatal("an expired idle lease must still be evicted")
	}
}
