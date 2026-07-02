package circadian

import (
	"context"
	"log/slog"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// DormantSessionManager is the narrow consumer-side interface used by both
// CircadianRhythm and MemoryLifecycleManager.
type DormantSessionManager interface {
	ListSessions(ctx context.Context, status domain.SessionStatus) ([]domain.Session, error)
	TransitionStatus(ctx context.Context, sessionID string, target domain.SessionStatus) error
}

// SessionEvictor clears expired session tokens (ADR-0018). Nil = no scavenging.
type SessionEvictor interface {
	EvictExpired()
}

// CircadianRhythm handles periodic session token eviction (ADR-0018).
// Session lifecycle consolidation has moved to MemoryLifecycleManager (ADR-0030).
type CircadianRhythm struct {
	SessionMgr          DormantSessionManager
	SessionEvictor      SessionEvictor
	SessionSweepInterval time.Duration

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

	if r.SessionEvictor == nil || r.SessionSweepInterval <= 0 {
		slog.Info("CircadianRhythm: session sweep disabled (no evictor or interval)")
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

// Stop signals the sweep goroutine to exit.
func (r *CircadianRhythm) Stop() {
	if r.stop != nil {
		r.stop()
	}
}
