package kernel

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/domain"
	memstore "github.com/cambrian-sh/core/internal/memory/store"
	"github.com/cambrian-sh/core/internal/supervision/aggregator"
	"github.com/cambrian-sh/core/internal/supervision/clusterer"

	"golang.org/x/sync/errgroup"
)

// SupervisionStack is the trust + metrics layer. It owns the ProfileAggregator
// (Merit recomputation) and the CapabilityClusterer (autonomous capability discovery).
//
// Biologically: this is the immune system's memory — remembering which cells
// (agents) are trustworthy, and the thalamus — gating information by domain.
type SupervisionStack struct {
	ProfileAggregator  *aggregator.ProfileAggregator
	Clusterer          *clusterer.CapabilityClusterer
}

// NewSupervisionStack constructs the trust layer.
//
// Parameters:
//   - reg:          the domain decorator (implements TaskEventReader + ClusterStore)
//   - profileStore: agent cognitive fingerprints (from MemoryStack)
//   - vecStore:     vector DB used to retrieve agent profile embeddings
//   - gen:          LLM generator for cluster naming (from AwarenessStack via kernel)
//   - cfg:          aggregator + clusterer tuning parameters
func NewSupervisionStack(
	reg *AgentRepoDecorator,
	profileStore memstore.ProfileStore,
	vecStore domain.VectorStore,
	gen domain.Generator,
	cfg *config.Config,
	observer domain.TelemetryObserver,
) *SupervisionStack {
	agg := aggregator.New(reg, profileStore, aggregator.AggregatorConfig{
		IntervalSeconds:     cfg.Execution.ProfileAggregatorIntervalSeconds,
		EWMAAlpha:           cfg.Execution.EWMAAlpha,
		LatencyWindow:       cfg.Execution.LatencyWindowSize,
		MinVerifiedEvents:   cfg.Execution.MinVerifiedEvents,
		TrustScoreCalWeight: cfg.Execution.TrustScoreCalWeight,
		TrustScoreAbsWeight: cfg.Execution.TrustScoreAbsWeight,
	})
	agg.Observer = observer

	src := &clusterAgentSource{reg: reg, vecStore: vecStore}
	c := clusterer.New(src, reg, gen,
		cfg.Execution.CapabilityClusterThreshold,
		cfg.Execution.CapabilityClusterEpsilon,
		cfg.Execution.CapabilityClusterMinAgents,
	)
	c.IntervalSeconds = cfg.Execution.CapabilityClusterIntervalSeconds

	return &SupervisionStack{
		ProfileAggregator: agg,
		Clusterer:         c,
	}
}

// Start launches both background workers concurrently.
func (s *SupervisionStack) Start(ctx context.Context) error {
	g, gCtx := errgroup.WithContext(ctx)
	g.Go(func() error { return s.ProfileAggregator.Start(gCtx) })
	g.Go(func() error { return s.Clusterer.Start(gCtx) })
	return g.Wait()
}

// Shutdown stops the supervision workers.
func (s *SupervisionStack) Shutdown(_ context.Context) {
	slog.Info("🛡️ SupervisionStack: shutdown complete")
}

// ── clusterAgentSource ────────────────────────────────────────────────────────

// clusterAgentSource is a kernel-layer adapter that satisfies clusterer.AgentSource
// by combining the agent registry (for Description/Trait/SourceHash) with the
// vector store (for embedding vectors stored alongside profiles).
type clusterAgentSource struct {
	reg      *AgentRepoDecorator
	vecStore domain.VectorStore
}

func (s *clusterAgentSource) GetAllAgentEmbeddings(ctx context.Context) ([]clusterer.AgentEmbedding, error) {
	agents, err := s.reg.GetAllAgents(ctx)
	if err != nil {
		return nil, fmt.Errorf("clusterAgentSource: list agents: %w", err)
	}

	// Build document IDs for a single-batch retrieval from pgvector.
	docIDs := make([]string, 0, len(agents))
	for _, a := range agents {
		docIDs = append(docIDs, fmt.Sprintf("profile:%s:%s", a.ID, a.SourceHash))
	}

	docs, err := s.vecStore.GetBatch(ctx, docIDs)
	if err != nil {
		return nil, fmt.Errorf("clusterAgentSource: GetBatch profiles: %w", err)
	}

	embByDocID := make(map[string][]float32, len(docs))
	for _, doc := range docs {
		embByDocID[doc.ID] = doc.Embedding.Vector
	}

	out := make([]clusterer.AgentEmbedding, 0, len(agents))
	for _, a := range agents {
		docID := fmt.Sprintf("profile:%s:%s", a.ID, a.SourceHash)
		emb := embByDocID[docID]
		if len(emb) == 0 {
			continue // not yet profiled — skip until Interview completes
		}
		out = append(out, clusterer.AgentEmbedding{
			AgentID:     a.ID,
			SourceHash:  a.SourceHash,
			Embedding:   emb,
			Description: a.Description,
			Trait:       string(a.Trait),
		})
	}
	return out, nil
}
