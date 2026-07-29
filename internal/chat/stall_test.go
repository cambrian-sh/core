package chat

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// blockingPool never returns until released — a wedged agent.
type blockingPool struct {
	release chan struct{}
	mu      sync.Mutex
	ctxErr  error
}

func (b *blockingPool) Dispatch(ctx context.Context, _ *domain.Handoff) (*domain.Handoff, error) {
	select {
	case <-b.release:
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("done")}}, nil
	case <-ctx.Done():
		b.mu.Lock()
		b.ctxErr = ctx.Err()
		b.mu.Unlock()
		return nil, ctx.Err()
	}
}

// A turn that reports nothing must be cut loose, and the user told why.
//
// This is the failure that prompted the fix: a wedged turn held its worker, its lease and
// the user's attention for the full TurnTimeout while the status line honestly reported
// "still searching memory" — truthful and useless.
func TestTurn_StalledTurnIsCancelledAndExplained(t *testing.T) {
	store := newMemStore()
	if err := store.CreateConversation(context.Background(), domain.Conversation{
		ID: "c1", OwnerID: "u1", Status: domain.ConversationOpen, Profile: domain.ProfileEmployee,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	pool := &blockingPool{release: make(chan struct{})}
	svc := NewTurnService(store, pool, nil)
	svc.StallTimeout = 150 * time.Millisecond
	svc.TurnTimeout = 30 * time.Second // deliberately far away: the STALL must fire first

	sink := &capturingSink{}
	svc.SetProgressSink(sink)
	// Liveness frozen at turn start — nothing ever reports again.
	frozen := time.Now()
	svc.SetLivenessProbe(func(string) time.Time { return frozen })

	start := time.Now()
	_, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "hi"})
	elapsed := time.Since(start)

	if !errors.Is(err, ErrTurnStalled) {
		t.Fatalf("expected ErrTurnStalled, got %v", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("stall detection should be prompt, took %v", elapsed)
	}
	// And the user is told, in words, what happened.
	last := sink.got[len(sink.got)-1]
	if !last.Final || !strings.Contains(last.Text(), "stopped making progress") {
		t.Errorf("expected a stall explanation, got %q", last.Text())
	}
	close(pool.release)
}

// A turn that is SLOW but still reporting must be left alone. Killing it would be the
// worst possible regression: the feature exists to make long work visible, not to kill it.
func TestTurn_SlowButReportingTurnIsNotKilled(t *testing.T) {
	store := newMemStore()
	if err := store.CreateConversation(context.Background(), domain.Conversation{
		ID: "c1", OwnerID: "u1", Status: domain.ConversationOpen, Profile: domain.ProfileEmployee,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	pool := &blockingPool{release: make(chan struct{})}
	svc := NewTurnService(store, pool, nil)
	svc.StallTimeout = 200 * time.Millisecond
	svc.TurnTimeout = 30 * time.Second
	// Liveness keeps advancing — the turn is working, just slowly.
	svc.SetLivenessProbe(func(string) time.Time { return time.Now() })

	go func() {
		time.Sleep(700 * time.Millisecond) // several stall windows of honest work
		close(pool.release)
	}()

	got, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "hi"})
	if err != nil {
		t.Fatalf("a reporting turn must not be killed: %v", err)
	}
	if got.Content != "done" {
		t.Errorf("unexpected reply %q", got.Content)
	}
}

// Without a probe the behaviour is exactly as before — stall detection is opt-in.
func TestTurn_WithoutALivenessProbeNothingIsCancelled(t *testing.T) {
	store := newMemStore()
	if err := store.CreateConversation(context.Background(), domain.Conversation{
		ID: "c1", OwnerID: "u1", Status: domain.ConversationOpen, Profile: domain.ProfileEmployee,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	pool := &blockingPool{release: make(chan struct{})}
	svc := NewTurnService(store, pool, nil)
	svc.StallTimeout = 100 * time.Millisecond // set, but no probe
	svc.TurnTimeout = 30 * time.Second

	go func() { time.Sleep(400 * time.Millisecond); close(pool.release) }()

	if _, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "hi"}); err != nil {
		t.Fatalf("without a probe the turn must run to completion: %v", err)
	}
}
