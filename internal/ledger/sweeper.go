package ledger

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// Sweeper enforces the ledger's lifetime (ADR-0145 D1): it purges entries whose
// retention ended (execution.ledger.retention_hours) every interval, and deletes a
// turn's scratch entries when the turn ends. One owner: started by Start, drained
// by Stop (AGENTS.md §3.3).
type Sweeper struct {
	store    domain.LedgerStore
	interval time.Duration
	now      func() time.Time

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// NewSweeper builds the sweeper over the raw store. interval <= 0 ⇒ one hour.
func NewSweeper(store domain.LedgerStore, interval time.Duration) *Sweeper {
	if interval <= 0 {
		interval = time.Hour
	}
	return &Sweeper{store: store, interval: interval, now: time.Now}
}

// Start runs one purge now and then one per interval until Stop or ctx ends.
// A second Start while running is a no-op.
func (s *Sweeper) Start(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.done = make(chan struct{})
	go func(done chan struct{}) {
		defer close(done)
		t := time.NewTicker(s.interval)
		defer t.Stop()
		for {
			s.Purge(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}(s.done)
}

// Stop ends the loop and waits for it.
func (s *Sweeper) Stop() {
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.cancel, s.done = nil, nil
	s.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

// Purge removes the expired entries once.
func (s *Sweeper) Purge(ctx context.Context) int {
	n, err := s.store.PurgeExpired(ctx, s.now())
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("ledger: retention purge failed", "err", err)
		}
		return n
	}
	if n > 0 {
		slog.Info("ledger: retention purge", "deleted", n)
	}
	return n
}

// EndTurn deletes the turn's scratch entries. Wired as the turn-end hook of the
// chat lane's delegation tracker; best-effort (an undeleted scratch row still
// expires with its retention).
func (s *Sweeper) EndTurn(ctx context.Context, turnID string) {
	if turnID == "" {
		return
	}
	if _, err := s.store.DeleteScratch(ctx, turnID); err != nil {
		slog.Warn("ledger: turn scratch not deleted; it expires with its retention", "turn", turnID, "err", err)
	}
}
