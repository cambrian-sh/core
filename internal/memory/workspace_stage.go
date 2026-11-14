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

// PrincipalRecall is the one recall WorkspaceStage reads through:
// QueryService.RecallFor (ENG-465, owner decision 2026-11-14). Consumer-side
// narrowing of *QueryService.
type PrincipalRecall interface {
	RecallFor(ctx context.Context, p domain.PrincipalRef, query, docType string) ([]domain.SearchResult, error)
}

// WorkspaceStageImpl enriches the Planner and DAGExecutor with cross-session LTM facts.
// It implements domain.WorkspaceStage. ADR-0016.
//
// ENG-465 (owner decision 2026-11-14): every lane reads AS THE PLANNING PRINCIPAL
// (domain.PrincipalFromContext — the authenticated caller of the Execute that is
// planning) through QueryService.RecallFor, so priming gets what that principal's
// own recall gets: the query-prefixed embedding, hybrid/RRF, the ADR-0136 fact lane
// with its current_fact / supersession features, the Stage-A blend, the relevance
// floor and the decision record. It used to search the store directly at
// ScopeSystem — every principal's rows primed every plan, superseded values
// included, embedded without the query prefix. No principal on ctx ⇒ no priming.
//
// ENG-540 (owner decision 2026-11-15) refines which lanes are personal: FACTS are read
// under the planning person's scope; PROCEDURES and NEGATIVE memories are global
// company knowledge, read as domain.SystemPrincipal (whose predicate keeps the owner
// fence, so a row tagged to one person never reaches another's plan). ADR-0034 records
// that those two kinds must hold no personal content. (Scenes, the fourth lane the
// decision names, were deleted 2026-11-05.)
type WorkspaceStageImpl struct {
	Recall PrincipalRecall
	// Embedder embeds the query for the ADR-0021 retrieval-session log only
	// (QueryEmbedding), through the shared embedQuery helper.
	Embedder               domain.Embedder
	Generator              domain.Generator
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
	recall PrincipalRecall,
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
		Recall:         recall,
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
	negatives := w.recallLane(ctx, taskQuery, domain.DocTypeNegativeEdge, 3, laneGlobal)

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

// laneScope says whose predicate a WorkspaceStage lane reads under (ENG-540).
type laneScope bool

const (
	lanePersonal laneScope = false // the planning principal's own scope
	laneGlobal   laneScope = true  // company-wide knowledge: domain.SystemPrincipal (owner fence kept)
)

// recallLane is one similarity lane, floored and capped. A personal lane reads as the
// planning principal; a global lane (procedures, negatives — ENG-540) reads as
// domain.SystemPrincipal. Either way there is no priming without a planning principal:
// global knowledge is primed FOR someone's plan, never on its own. Best-effort: a
// failure costs enrichment, never the plan.
func (w *WorkspaceStageImpl) recallLane(ctx context.Context, query, docType string, max int, scope laneScope) []domain.SearchResult {
	if w.Recall == nil {
		return nil
	}
	p := domain.PrincipalFromContext(ctx)
	if p.ID == "" {
		return nil
	}
	if scope == laneGlobal {
		// ENG-540: explicit, greppable global read — procedures and negative memories
		// are company knowledge and must hold no personal content (ADR-0034).
		p = domain.SystemPrincipal
	}
	hits, err := w.Recall.RecallFor(ctx, p, query, docType)
	if err != nil {
		slog.WarnContext(ctx, "WorkspaceStage: recall failed", "doc_type", docType, "err", err)
		return nil
	}
	hits = w.floored(hits)
	if len(hits) > max {
		hits = hits[:max]
	}
	return hits
}

// floored drops dense hits whose raw cosine is below RetrievalFloor
// (execution.retrieval.retrieval_floor, which the direct store search used to
// apply in SQL). Rows with no cosine (RawScore == 0: lexical- or fact-lane-only
// hits) are kept — the same rule QueryService's own relevance floor follows, so a
// hybrid hit is never dropped for lacking a dense score.
func (w *WorkspaceStageImpl) floored(hits []domain.SearchResult) []domain.SearchResult {
	if w.RetrievalFloor <= 0 {
		return hits
	}
	out := hits[:0:0]
	for _, h := range hits {
		if h.RawScore > 0 && h.RawScore < w.RetrievalFloor {
			continue
		}
		out = append(out, h)
	}
	return out
}

func (w *WorkspaceStageImpl) enrich(ctx context.Context, query string, slots int, caller string) ([]domain.SearchResult, error) {
	// ENG-465: no principal on ctx ⇒ no priming (owner decision 2026-11-14). There
	// is no kernel-wide fallback read any more.
	p := domain.PrincipalFromContext(ctx)
	if p.ID == "" || w.Recall == nil {
		slog.InfoContext(ctx, "WorkspaceStage: no planning principal; no priming", "caller", caller)
		return nil, nil
	}
	// The ADR-0016 §4 SCENE query priming was removed 2026-11-05 with scenes
	// (decisions/scenes-cancelled.md).
	results, err := w.Recall.RecallFor(ctx, p, query, domain.DocTypeMnemonicFact)
	if err != nil {
		slog.Warn("WorkspaceStage: FACT query failed", "caller", caller, "err", err)
		return nil, nil
	}
	results = w.floored(results)

	factHits := len(results)
	if factHits == 0 {
		slog.Info("WorkspaceStage: cold start — no FACT documents",
			"workspace_cold_start", true, "fact_hits", 0, "caller", caller)
		return nil, nil
	}

	// ADR-0017 spreading over document_edges now runs inside the recall pass
	// (QueryService.EnableSpreading, execution.retrieval.recall_spreading_enabled),
	// read as the same principal; spreading here again would add the expansion twice
	// and outside the principal's read.

	// MinFactCosine filter (PLANNERREQ REQ1): discard facts whose raw cosine to the
	// query is below the threshold. Rows with no cosine (lexical- or fact-lane-only,
	// RawScore == 0) are kept, as QueryService's floor keeps them.
	if w.MinFactCosine > 0 {
		filtered := results[:0]
		dropped := 0
		for _, r := range results {
			if r.RawScore == 0 || r.RawScore >= w.MinFactCosine {
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
		var queryVec []float32
		if w.Embedder != nil {
			queryVec, _ = embedQuery(ctx, w.Embedder, query) // best-effort: the log row is kept without it
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
	hits := w.recallLane(ctx, query, domain.DocTypeMnemonicProcedure, 3, laneGlobal) // a planner needs the best routine, not a catalogue
	if len(hits) == 0 {
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
