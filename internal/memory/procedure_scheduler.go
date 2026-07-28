package memory

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ProcedureScheduler runs the ADR-0094 D3 induction pass on an interval.
//
// This is the machinery ADR-0049 §A2.5 had to permit before it could exist — that ADR
// originally rejected batch consolidation outright. The permission came with
// constraints, and they are what this type is shaped around:
//
//   - Nothing load-bearing depends on it. The online path is fully correct if this never
//     runs; a missed pass degrades enrichment, never recall or planning. So every failure
//     here is logged and swallowed, and the loop keeps its cadence.
//   - It produces only ABSTRACTIONS. It reads completed episodes and writes procedures.
//     It never repairs, backfills or rewrites a primary record.
//   - It is idempotent and resumable. Procedure ids derive from cluster shape, so a
//     re-run updates in place rather than minting duplicates.
//
// The original rejection was aimed at a consolidation pass the GRAPH depended on, which
// therefore broke the graph when it did not fire. Nothing here is depended upon, which is
// precisely why it is safe to run offline.
type ProcedureScheduler struct {
	Inducer *ProcedureInducer
	Store   domain.VectorStore
	// Interval between passes. Zero disables the scheduler entirely.
	Interval time.Duration
	// MaxEpisodes bounds one pass's read. Induction is O(episodes) and runs off the
	// hot path, but an unbounded scan over a store that grows forever is a latent
	// incident rather than a design.
	MaxEpisodes int
}

// Start launches the loop. Non-blocking; returns immediately. The loop exits on ctx
// cancellation, so it shuts down with the kernel and needs no separate Stop.
func (s *ProcedureScheduler) Start(ctx context.Context) {
	if s == nil || s.Interval <= 0 || s.Inducer == nil || s.Store == nil {
		return // disabled: the default, and a no-op rather than an error
	}
	go s.loop(ctx)
	slog.Info("ADR-0094: procedure induction scheduled", "interval", s.Interval)
}

func (s *ProcedureScheduler) loop(ctx context.Context) {
	// Jitter the FIRST pass across up to a tenth of the interval. Several kernels
	// sharing one database would otherwise all induce at the same instant after a
	// coordinated restart, turning a background job into a thundering herd against the
	// store. Full-jitter on the first tick only; the cadence itself stays regular so
	// the pass remains predictable to an operator watching for it.
	first := time.Duration(rand.Int64N(int64(s.Interval/10) + 1))
	select {
	case <-ctx.Done():
		return
	case <-time.After(first):
	}

	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()
	for {
		s.runOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runOnce performs a single induction pass. Every failure is logged and swallowed: this
// is enrichment, and a scheduler that died on a transient store error would silently
// stop producing procedures with no signal beyond their absence.
func (s *ProcedureScheduler) runOnce(ctx context.Context) {
	limit := s.MaxEpisodes
	if limit <= 0 {
		limit = 500
	}
	// Read completed episodes by TYPE rather than by a time cursor. The pass is
	// idempotent, so re-reading an episode it has already seen costs a little work and
	// changes nothing — whereas a cursor would need durable state of its own, and
	// A2.5's "nothing load-bearing may depend on it" argues against giving a
	// best-effort job persistent state to get wrong.
	docs, err := s.Store.QueryByMetadata(ctx, map[string]string{"outcome": "success"}, limit)
	if err != nil {
		slog.WarnContext(ctx, "ADR-0094: induction pass could not read episodes", "err", err)
		return
	}
	episodes := EpisodesFromScenes(docs)
	if len(episodes) == 0 {
		slog.DebugContext(ctx, "ADR-0094: no inducible episodes this pass")
		return
	}
	written, err := s.Inducer.Induce(ctx, episodes)
	if err != nil {
		slog.WarnContext(ctx, "ADR-0094: induction pass failed", "err", err)
		return
	}
	if written > 0 {
		slog.InfoContext(ctx, "ADR-0094: procedures induced",
			"procedures", written, "episodes_scanned", len(episodes))
	}
}
