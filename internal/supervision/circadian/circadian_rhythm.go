package circadian

import (
	"context"
	"log/slog"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// DormantSessionManager is the narrow consumer-side interface used by both
// CircadianRhythm and MemoryLifecycleManager.
type DormantSessionManager interface {
	ListSessions(ctx context.Context, status domain.SessionStatus) ([]domain.Session, error)
	TransitionStatus(ctx context.Context, sessionID domain.SessionID, target domain.SessionStatus) error
}

// SessionEvictor clears expired per-step BudgetLeases (ADR-0018). Nil = no scavenging.
//
// NOTE the two different "session" sweeps on this type: this one reclaims LEASES (lifetime:
// one step), while IdleSweeper below ages out task SESSIONS (lifetime: days). They were
// never the same thing; only one of them existed before Phase 2.
type SessionEvictor interface {
	EvictExpired()
}

// IdleSweeper ages ACTIVE sessions into DORMANT once they have been untouched for
// SessionIdleTimeout. Satisfied by *session.SessionManager. Nil = no session ageing.
type IdleSweeper interface {
	SweepIdle(ctx context.Context, idleFor time.Duration) (int, error)
}

// RetentionPurger reclaims COMPLETED sessions older than the retention window, cascading to
// their runs and checkpoints. Satisfied by *postgres.PgSessionStore. Nil = never reclaim.
//
// This is the third and last stage of the lifecycle, and the one that actually bounds
// growth: ageing a session to dormant and then completing it means nothing if completed
// rows accumulate forever.
type RetentionPurger interface {
	PurgeCompletedBefore(ctx context.Context, cutoff time.Time) (int64, error)
	PurgeOrphanRunsBefore(ctx context.Context, cutoff time.Time) (int64, error)
}

// CircadianRhythm handles periodic session token eviction (ADR-0018).
// Session lifecycle consolidation has moved to MemoryLifecycleManager (ADR-0030).
type CircadianRhythm struct {
	SessionMgr           DormantSessionManager
	SessionEvictor       SessionEvictor
	SessionSweepInterval time.Duration

	// Phase 2 — task-session ageing (see IdleSweeper). A zero timeout or interval, or a
	// nil sweeper, disables it.
	IdleSweeper              IdleSweeper
	SessionIdleTimeout       time.Duration
	SessionIdleSweepInterval time.Duration

	// Phase 4 — retention. A zero window or interval, or a nil purger, disables it.
	RetentionPurger        RetentionPurger
	SessionRetention       time.Duration
	RetentionSweepInterval time.Duration

	stop context.CancelFunc
}

func New(sessionMgr DormantSessionManager, _ int) *CircadianRhythm {
	return &CircadianRhythm{
		SessionMgr: sessionMgr,
	}
}

// Start launches the session-token sweep goroutine. Session lifecycle
// consolidation is handled by MemoryLifecycleManager (ADR-0030).
func (r *CircadianRhythm) Start(ctx context.Context) {
	ctx, r.stop = context.WithCancel(ctx)
	r.startLeaseSweep(ctx)
	r.startIdleSessionSweep(ctx)
	r.startRetentionSweep(ctx)
}

// startRetentionSweep reclaims completed sessions past the retention window (Phase 4).
func (r *CircadianRhythm) startRetentionSweep(ctx context.Context) {
	if r.RetentionPurger == nil || r.SessionRetention <= 0 || r.RetentionSweepInterval <= 0 {
		slog.Info("CircadianRhythm: retention sweep disabled",
			"retention", r.SessionRetention, "interval", r.RetentionSweepInterval)
		return
	}
	ticker := time.NewTicker(r.RetentionSweepInterval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cutoff := time.Now().Add(-r.SessionRetention)
				sessions, err := r.RetentionPurger.PurgeCompletedBefore(ctx, cutoff)
				if err != nil {
					slog.Warn("CircadianRhythm: session retention sweep failed", "err", err)
					continue
				}
				runs, err := r.RetentionPurger.PurgeOrphanRunsBefore(ctx, cutoff)
				if err != nil {
					slog.Warn("CircadianRhythm: orphan-run sweep failed", "err", err)
				}
				if sessions > 0 || runs > 0 {
					slog.Info("CircadianRhythm: reclaimed expired sessions",
						"sessions", sessions, "orphan_runs", runs, "retention", r.SessionRetention)
				}
			}
		}
	}()
}

// startLeaseSweep reclaims expired per-step BudgetLeases (ADR-0018).
func (r *CircadianRhythm) startLeaseSweep(ctx context.Context) {
	if r.SessionEvictor == nil || r.SessionSweepInterval <= 0 {
		slog.Info("CircadianRhythm: lease sweep disabled (no evictor or interval)")
		return
	}
	sweepTicker := time.NewTicker(r.SessionSweepInterval)
	go func() {
		defer sweepTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-sweepTicker.C:
				r.SessionEvictor.EvictExpired()
			}
		}
	}()
}

// startIdleSessionSweep ages idle ACTIVE sessions into DORMANT (Phase 2).
func (r *CircadianRhythm) startIdleSessionSweep(ctx context.Context) {
	if r.IdleSweeper == nil || r.SessionIdleTimeout <= 0 || r.SessionIdleSweepInterval <= 0 {
		slog.Info("CircadianRhythm: idle session sweep disabled",
			"idle_timeout", r.SessionIdleTimeout, "interval", r.SessionIdleSweepInterval)
		return
	}
	ticker := time.NewTicker(r.SessionIdleSweepInterval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				n, err := r.IdleSweeper.SweepIdle(ctx, r.SessionIdleTimeout)
				if err != nil {
					slog.Warn("CircadianRhythm: idle session sweep failed", "err", err)
					continue
				}
				if n > 0 {
					slog.Info("CircadianRhythm: sessions aged to dormant",
						"count", n, "idle_timeout", r.SessionIdleTimeout)
				}
			}
		}
	}()
}

// Stop signals the sweep goroutine to exit.
func (r *CircadianRhythm) Stop() {
	if r.stop != nil {
		r.stop()
	}
}
