package kernel

import (
	"context"
	"log/slog"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	memstore "github.com/cambrian-sh/core/internal/memory/store"
	"github.com/cambrian-sh/core/internal/supervision/aggregator"

	"golang.org/x/sync/errgroup"
)

// SupervisionStack is the trust + metrics layer. It owns the ProfileAggregator
// (Merit recomputation).
//
// ROUTE-04 / ADR-0067: the LLM CapabilityClusterer was RETIRED — it overwrote each
// agent's declared capabilities with an invented cluster name unrelated to the
// manifest vocabulary that ROUTE-03's L1 and the planner actually enforce, so it was
// redundant work at best and a source of vocabulary divergence at worst. Capabilities
// are now the ones agents DECLARE (manifest.Capabilities), optionally folded by
// deterministic normalization (execution.canonical_vocab) — no clustering.
//
// Biologically: this is the immune system's memory — remembering which cells
// (agents) are trustworthy.
type SupervisionStack struct {
	ProfileAggregator *aggregator.ProfileAggregator
}

// NewSupervisionStack constructs the trust layer. vecStore and gen are retained in the
// signature (no caller change) but no longer used since the clusterer's retirement.
func NewSupervisionStack(
	reg *AgentRepoDecorator,
	profileStore memstore.ProfileStore,
	_ domain.VectorStore,
	_ domain.Generator,
	cfg *config.Config,
	observer domain.TelemetryObserver,
) *SupervisionStack {
	agg := aggregator.New(reg, profileStore, aggregator.AggregatorConfig{
		IntervalSeconds:     cfg.Execution.Supervision.ProfileAggregatorIntervalSeconds,
		EWMAAlpha:           cfg.Execution.Supervision.EWMAAlpha,
		LatencyWindow:       cfg.Execution.Supervision.LatencyWindowSize,
		MinVerifiedEvents:   cfg.Execution.Verification.MinVerifiedEvents,
		TrustScoreCalWeight: cfg.Execution.Verification.TrustScoreCalWeight,
		TrustScoreAbsWeight: cfg.Execution.Verification.TrustScoreAbsWeight,
	})
	agg.Observer = observer

	return &SupervisionStack{ProfileAggregator: agg}
}

// Start launches the background workers.
func (s *SupervisionStack) Start(ctx context.Context) error {
	g, gCtx := errgroup.WithContext(ctx)
	g.Go(func() error { return s.ProfileAggregator.Start(gCtx) })
	return g.Wait()
}

// Shutdown stops the supervision workers.
func (s *SupervisionStack) Shutdown(_ context.Context) {
	slog.Info("🛡️ SupervisionStack: shutdown complete")
}
