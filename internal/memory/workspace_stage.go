package memory

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// WorkspaceStageImpl enriches the Planner and DAGExecutor with cross-session LTM facts.
// It implements domain.WorkspaceStage. ADR-0016.
type WorkspaceStageImpl struct {
	Store                  domain.VectorStore
	Embedder               domain.Embedder
	Generator              domain.Generator
	SpreadingEngine        *SpreadingEngine // ADR-0017: may be nil; nil disables spreading
	PlanningSlots          int
	ExecutionSlots         int
	RetrievalFloor         float64
	RetrievalSessionLogger domain.RetrievalSessionLogger // ADR-0021: may be nil
	// MinFactCosine is the minimum raw cosine similarity (pre-floor-multiplier) a FACT
	// document must have against the query to be injected into the Planner prompt.
	// Default 0.60; wired from config.WorkspaceMinFactCosine at composition root.
	// PLANNERREQ REQ1: eliminates low-relevance documents that pass the broad retrieval floor.
	MinFactCosine float64

	// The episodic retrieval lane (ADR-0029) used to live here, gated on a
	// PolicyProvider that resolved the "episodic" HippocampusPolicy. Its WRITER — the
	// EpisodicExtractor / ConsolidatorAgent — was removed on 2026-07-18, so nothing has
	// written a DocTypeEpisodicMemory row since; the lane was a goroutine and a store
	// search (with a nil query vector, which pgvector does not reject) issued on every
	// planning pass against a corpus that could only shrink. Removed with the field:
	// there is no configuration that makes it produce a hit. The "episodic" policy
	// default followed on 2026-10-31.

}

// NewWorkspaceStage creates a new WorkspaceStageImpl.
func NewWorkspaceStage(
	store domain.VectorStore,
	embedder domain.Embedder,
	gen domain.Generator,
	planningSlots, executionSlots int,
	retrievalFloor float64,
) *WorkspaceStageImpl {
	if planningSlots <= 0 {
		planningSlots = 10
	}
	if executionSlots <= 0 {
		executionSlots = 5
	}
	if retrievalFloor <= 0 {
		retrievalFloor = 0.2
	}
	return &WorkspaceStageImpl{
		Store:          store,
		Embedder:       embedder,
		Generator:      gen,
		PlanningSlots:  planningSlots,
		ExecutionSlots: executionSlots,
		RetrievalFloor: retrievalFloor,
	}
}

// PrimeForPlanning retrieves cross-session FACT + NegativeEdge documents and stored
// procedures for the Planner. ADR-0025: returns LTMEnrichment (typed).
func (w *WorkspaceStageImpl) PrimeForPlanning(ctx context.Context, taskQuery string) (domain.LTMEnrichment, error) {
	facts, err := w.enrich(ctx, taskQuery, w.PlanningSlots, "planning")
	if err != nil {
		return domain.LTMEnrichment{}, err
	}
	// The negatives lane is a SIMILARITY lane like every other one. It used to search
	// with a nil vector, which the pgvector adapter does not reject: it scores every row
	// identically and orders by last_accessed_at, so the three most recently touched
	// failures entered every planner prompt whatever the task was. Scored against the
	// situation and floored like the procedure lane, for the same reason — an unrelated
	// failure is a fabricated analogy, and a prompt padded with one is worse than a lane
	// that returns nothing.
	//
	// Best-effort, as before: a failure here costs enrichment, never the plan.
	var negatives []domain.SearchResult
	if negVec, embedErr := w.Embedder.Embed(ctx, taskQuery); embedErr == nil {
		negatives, _ = w.Store.Search(ctx, negVec, domain.SearchOptions{
			DocumentType:   domain.DocTypeNegativeEdge,
			TopK:           3,
			RetrievalFloor: w.RetrievalFloor,
			Scope:          domain.ScopeSystem, // ADR-0034: workspace enrichment is a kernel read
		})
	}

	enrichment := domain.LTMEnrichment{Facts: facts, Negatives: negatives}
	// The ADR-0049 D11 precedent lane (scene transitions) was removed 2026-11-05 with
	// scenes (decisions/scenes-cancelled.md).
	// ADR-0094 D5: stored routines for this situation.
	enrichment.Procedures = w.retrieveProcedureLane(ctx, taskQuery)
	return enrichment, nil
}

// PrimeForExecution retrieves cross-session FACT documents for the DAGExecutor.
func (w *WorkspaceStageImpl) PrimeForExecution(ctx context.Context, plan *domain.ExecutionPlan, initialContext map[string]string) (map[string]string, error) {
	query := plan.Subject
	if query == "" && len(plan.Steps) > 0 {
		query = plan.Steps[0].Query
	}
	results, err := w.enrich(ctx, query, w.ExecutionSlots, "execution")
	if err != nil {
		return map[string]string{}, err
	}
	return resultsToMap(results), nil
}

// resultsToMap converts search results to the legacy ltm_fact_N map format for PrimeForExecution.
func resultsToMap(results []domain.SearchResult) map[string]string {
	m := make(map[string]string, len(results))
	for i, r := range results {
		key := fmt.Sprintf("ltm_fact_%d", i)
		m[key] = r.Document.Text
	}
	return m
}

func (w *WorkspaceStageImpl) enrich(ctx context.Context, query string, slots int, caller string) ([]domain.SearchResult, error) {
	// 1. Embed raw query.
	rawVec, err := w.Embedder.Embed(ctx, query)
	if err != nil {
		slog.Warn("WorkspaceStage: embed failed", "caller", caller, "err", err)
		return nil, nil
	}

	// 2. The ADR-0016 §4 SCENE query priming (blend the query vector with the nearest
	// `mnemonic_scene`, guarded by a drift threshold) was removed 2026-11-05 with
	// scenes (decisions/scenes-cancelled.md). The FACT query uses the raw embedding.
	// An A/B/A planner-retrieval check of that removal is ticketed (AGENTS.md §0.5).
	queryVec := rawVec

	// 3. FACT query.
	overFetch := slots * 3
	if overFetch < 10 {
		overFetch = 10
	}
	results, err := w.Store.Search(ctx, queryVec, domain.SearchOptions{
		DocumentType:   domain.DocTypeMnemonicFact,
		TopK:           overFetch,
		RetrievalFloor: w.RetrievalFloor,
		Scope:          domain.ScopeSystem, // ADR-0034: workspace enrichment is a kernel read
	})
	if err != nil {
		slog.Warn("WorkspaceStage: FACT query failed", "caller", caller, "err", err)
		return nil, nil
	}

	factHits := len(results)
	if factHits == 0 {
		slog.Info("WorkspaceStage: cold start — no FACT documents",
			"workspace_cold_start", true, "fact_hits", 0, "caller", caller)
		return nil, nil
	}

	// ADR-0017: Spread activation from FACT hits through document_edges graph.
	if w.SpreadingEngine != nil && factHits > 0 {
		expansions := w.SpreadingEngine.Spread(ctx, results)
		// Merge graph-discovered nodes into results.
		seen := make(map[string]bool)
		for _, r := range results {
			seen[r.Document.ID] = true
		}
		for _, exp := range expansions {
			if !seen[exp.Document.ID] {
				results = append(results, domain.SearchResult{
					Document: exp.Document,
					Score:    exp.ActivationEnergy,
				})
				seen[exp.Document.ID] = true
			}
		}
	}

	// 5. MinFactCosine filter (PLANNERREQ REQ1).
	// Discard facts whose raw cosine similarity to the query is below the threshold.
	// This removes low-relevance documents that pass the broad RetrievalFloor.
	if w.MinFactCosine > 0 {
		filtered := results[:0]
		dropped := 0
		for _, r := range results {
			if r.RawScore >= w.MinFactCosine {
				filtered = append(filtered, r)
			} else {
				dropped++
			}
		}
		if dropped > 0 {
			slog.Info("WorkspaceStage: MinFactCosine filter applied",
				"dropped", dropped, "remaining", len(filtered), "threshold", w.MinFactCosine, "caller", caller)
		}
		results = filtered
	}

	// 6. Truncation.
	truncated := false
	if len(results) > slots {
		truncated = true
		results = results[:slots]
	}
	if truncated {
		slog.Info("WorkspaceStage: slot truncation",
			"workspace_slots_truncated", true, "available_docs", len(results), "slots", slots, "caller", caller)
	}

	// ADR-0021: Log retrieval session for deferred LTR / nDCG@K data collection.
	if w.RetrievalSessionLogger != nil {
		retrievedDocs := make([]domain.RetrievedDoc, len(results))
		for i, r := range results {
			retrievedDocs[i] = domain.RetrievedDoc{
				DocID:              r.Document.ID,
				Score:              r.Score,
				ActivationStrength: r.Document.ActivationStrength,
				DocType:            r.Document.DocumentType,
				Rank:               i + 1,
			}
		}
		isExploration := false
		if n, err := rand.Int(rand.Reader, big.NewInt(100)); err == nil && n.Int64() < 5 {
			isExploration = true
		}
		session := domain.RetrievalSession{
			SessionID:       fmt.Sprintf("ret-%d", time.Now().UnixNano()),
			Query:           query,
			QueryEmbedding:  queryVec,
			Caller:          caller,
			FactHits:        factHits,
			RetrievedDocs:   retrievedDocs,
			Truncated:       truncated,
			ExplorationSlot: isExploration,
			Timestamp:       time.Now(),
		}
		if err := w.RetrievalSessionLogger.LogRetrieval(session); err != nil {
			slog.Warn("WorkspaceStage: failed to log retrieval session", "caller", caller, "err", err)
		}
	}

	slog.Info("WorkspaceStage: enrichment complete",
		"caller", caller, "workspace_cold_start", false, "fact_hits", len(results))
	return results, nil
}

// retrieveProcedureLane finds induced routines whose TRIGGER matches the situation being
// planned (ADR-0094 D5).
//
// Similarity-gated by RetrievalFloor, for a sharp reason:
// a procedure retrieved for a situation it does not fit is worse than no procedure at
// all — it is a confident wrong answer, and a plausible-looking list of steps is exactly
// the kind of wrong a planner will follow.
//
// DEPRECATED and SUPERSEDED routines are excluded here rather than merely deranked. They
// already sink to the activation floor (procedureActivation), but a retired routine
// reaching a prompt at all would undo the point of retiring it, and "it scored low" is a
// weaker guarantee than "it was not returned".
func (w *WorkspaceStageImpl) retrieveProcedureLane(ctx context.Context, query string) []domain.Procedure {
	if w.Store == nil || w.Embedder == nil {
		return nil
	}
	vec, err := w.Embedder.Embed(ctx, query)
	if err != nil {
		return nil
	}
	hits, err := w.Store.Search(ctx, vec, domain.SearchOptions{
		DocumentType:   domain.DocTypeMnemonicProcedure,
		TopK:           3, // a planner needs the best routine, not a catalogue
		RetrievalFloor: w.RetrievalFloor,
		Scope:          domain.ScopeSystem,
	})
	if err != nil || len(hits) == 0 {
		return nil
	}
	out := make([]domain.Procedure, 0, len(hits))
	for _, h := range hits {
		p, ok := procedureFromDoc(h.Document)
		if !ok || p.Status != domain.ProcedureActive {
			continue
		}
		out = append(out, p)
	}
	return out
}
