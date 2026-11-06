package lifecycle

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// Curate runs one deterministic curator pass at now (ADR-0139 D13: no LLM; the
// only actor that moves skills between active, stale and archived on its own).
// For every record whose source the curator manages and that is not exempt:
//
//	active   idle ≥ StaleAfter                 → stale
//	stale    idle ≥ ArchiveAfter               → archived
//	stale    used since it went stale          → active
//
// where idle = now − max(LastUsedAt, CurrentSince). Archived is never deleted
// and never reactivated by the curator (an archived skill is not served, so it
// cannot be used); an operator rollback restores it. Returns the transitions made.
func (s *Service) Curate(ctx context.Context, now time.Time) (int, error) {
	c := s.cfg.Curator
	if c.StaleAfter <= 0 || c.ArchiveAfter <= c.StaleAfter {
		return 0, errors.New("skill curator: need 0 < stale_after < archive_after")
	}
	managed := map[string]bool{}
	for _, src := range c.Sources {
		managed[src] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	recs, err := s.store.ListSkills(ctx)
	if err != nil {
		return 0, err
	}
	moved := 0
	var firstErr error
	for _, rec := range recs {
		if !managed[rec.Source] || s.ExemptReason(rec.Name) != "" {
			continue
		}
		last := rec.CurrentSince
		if rec.LastUsedAt.After(last) {
			last = rec.LastUsedAt
		}
		idle := now.Sub(last)
		to := ""
		switch rec.State {
		case domain.SkillStateActive:
			if idle >= c.StaleAfter {
				to = domain.SkillStateStale
			}
		case domain.SkillStateStale:
			switch {
			case rec.LastUsedAt.After(rec.StateChangedAt):
				to = domain.SkillStateActive
			case idle >= c.ArchiveAfter:
				to = domain.SkillStateArchived
			}
		}
		if to == "" {
			continue
		}
		head, err := s.store.LedgerHead(ctx, rec.Name)
		if err != nil {
			firstErr = errors.Join(firstErr, err)
			continue
		}
		next := rec
		next.State, next.StateChangedAt = to, now.UTC()
		e, err := s.store.Commit(ctx, domain.SkillCommit{
			ExpectSeq: head, Record: &next,
			Entry: domain.SkillLedgerEntry{
				SkillName: rec.Name, Mutation: domain.SkillMutationStateChanged,
				FromVersion: rec.Version, ToVersion: rec.Version, FromState: rec.State, ToState: to,
				Locked: rec.Locked, Actor: domain.SkillActorCurator,
				Reason: "idle " + idle.Truncate(time.Hour).String(), At: now.UTC(),
			},
		})
		if err != nil {
			// A conflict means someone else touched the skill this pass; the next
			// pass re-reads it. Not an error worth failing the pass for.
			if !errors.Is(err, domain.ErrSkillLedgerConflict) {
				firstErr = errors.Join(firstErr, err)
			}
			continue
		}
		next.LedgerSeq, next.LedgerEntryID = e.Seq, e.ID
		if to == domain.SkillStateArchived {
			_ = s.apply(ctx, next) // unregister + index removal; apply logs its own failures
		}
		slog.InfoContext(ctx, "ADR-0139 D13: curator moved skill", "event", "skill_state_changed",
			"skill", rec.Name, "from", rec.State, "to", to)
		moved++
	}
	return moved, firstErr
}

// Curator is the background worker that runs Curate on an interval. One owner,
// started by Start and drained by Stop (AGENTS.md §3.3).
type Curator struct {
	svc      *Service
	interval time.Duration
	now      func() time.Time

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// NewCurator builds the worker over svc. interval ≤ 0 uses svc's configured
// interval (default 1h). now nil = the service clock.
func NewCurator(svc *Service, interval time.Duration, now func() time.Time) *Curator {
	if interval <= 0 {
		interval = svc.cfg.Curator.Interval
	}
	if interval <= 0 {
		interval = time.Hour
	}
	if now == nil {
		now = svc.now
	}
	return &Curator{svc: svc, interval: interval, now: now}
}

// Start launches the loop; a second Start is a no-op. A pass runs immediately,
// then every interval.
func (c *Curator) Start(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	c.cancel, c.done = cancel, make(chan struct{})
	go c.loop(ctx, c.done)
}

func (c *Curator) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		if _, err := c.svc.Curate(ctx, c.now()); err != nil && ctx.Err() == nil {
			slog.Warn("ADR-0139 D13: curator pass failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Stop cancels the loop and waits for it to exit. Safe without Start.
func (c *Curator) Stop() {
	c.mu.Lock()
	cancel, done := c.cancel, c.done
	c.cancel, c.done = nil, nil
	c.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}
