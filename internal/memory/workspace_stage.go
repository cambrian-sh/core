package memory

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"math/big"
	"sync"
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
	DriftGuard             bool
	DriftThreshold         float64
	RetrievalSessionLogger domain.RetrievalSessionLogger // ADR-0021: may be nil
	// MinFactCosine is the minimum raw cosine similarity (pre-floor-multiplier) a FACT
	// document must have against the query to be injected into the Planner prompt.
	// Default 0.60; wired from config.WorkspaceMinFactCosine at composition root.
	// PLANNERREQ REQ1: eliminates low-relevance documents that pass the broad retrieval floor.
	MinFactCosine float64

	// PolicyProvider resolves named HippocamupusPolicies for the episodic retrieval lane.
	// When nil, the episodic lane is skipped and LTMEnrichment.Episodes stays empty. ADR-0029.
	PolicyProvider domain.PolicyProvider

	// ActivationThreshold is the post-BFS selection floor for PrimeForStep (ADR-0022).
	// Must NOT equal RetrievalFloor — they operate at different scales.
	// Default 0.1; wired from config.ActivationThreshold at composition root.
	ActivationThreshold float64
	// LRUCacheCapacity is the maximum number of cached query entries in PrimeForStep's
	// embedding and ContextRef LRU caches. 0 falls back to the hardcoded default (100).
	// Wired from config.WorkspaceLRUCacheCapacity at composition root. ADR-0022.
	LRUCacheCapacity int

	// ADR-0022 Phase 2B: caches.
	cacheMu    sync.Mutex
	refCache   *lruCache[string, []domain.ContextRef] // query → refs (invalidated on Tier-2 drain)
	embedCache *lruCache[string, []float32]           // query → embedding (TTL-free: deterministic)
}

// NewWorkspaceStage creates a new WorkspaceStageImpl.
func NewWorkspaceStage(
	store domain.VectorStore,
	embedder domain.Embedder,
	gen domain.Generator,
	planningSlots, executionSlots int,
	retrievalFloor float64,
	driftGuard bool,
	driftThreshold float64,
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
	if driftThreshold <= 0 {
		driftThreshold = 0.7
	}
	return &WorkspaceStageImpl{
		Store:          store,
		Embedder:       embedder,
		Generator:      gen,
		PlanningSlots:  planningSlots,
		ExecutionSlots: executionSlots,
		RetrievalFloor: retrievalFloor,
		DriftGuard:     driftGuard,
		DriftThreshold: driftThreshold,
	}
}

// SetActivationThreshold updates the BFS selection floor at runtime.
// Used by the threshold sensitivity sweep benchmark (phase2_validation_test.go).
func (w *WorkspaceStageImpl) SetActivationThreshold(v float64) {
	w.ActivationThreshold = v
}

// PrimeForPlanning retrieves cross-session FACT + SCENE + NegativeEdge + EpisodicMemory
// documents for the Planner. Three parallel query lanes run concurrently.
// ADR-0025: returns LTMEnrichment (typed). ADR-0029: adds Episodes lane.
func (w *WorkspaceStageImpl) PrimeForPlanning(ctx context.Context, taskQuery string) (domain.LTMEnrichment, error) {
	// Episodic lane runs in parallel with the FACT+SCENE enrichment.
	type episodicResult struct {
		hits []domain.SearchResult
	}
	episodicCh := make(chan episodicResult, 1)
	if w.PolicyProvider != nil {
		go func() {
			policy, ok := w.PolicyProvider.GetPolicy("episodic")
			if !ok {
				policy = w.PolicyProvider.DefaultPolicy()
			}
			topK := 3
			if topK < 1 {
				topK = 1
			}
			results, err := w.Store.Search(ctx, nil, domain.SearchOptions{
				DocumentType: domain.DocTypeEpisodicMemory,
				TopK:         topK,
				Scope:        domain.ScopeSystem, // ADR-0034: workspace enrichment is a kernel read
			})
			if err != nil {
				slog.Warn("WorkspaceStage: episodic lane search failed", "err", err)
				episodicCh <- episodicResult{}
				return
			}
			var above []domain.SearchResult
			for _, r := range results {
				if r.Score >= policy.SimilarityThreshold {
					above = append(above, r)
				}
			}
			episodicCh <- episodicResult{hits: above}
		}()
	}

	facts, err := w.enrich(ctx, taskQuery, w.PlanningSlots, true, "planning")
	if err != nil {
		return domain.LTMEnrichment{}, err
	}
	negatives, _ := w.Store.Search(ctx, nil, domain.SearchOptions{
		DocumentType: domain.DocTypeNegativeEdge,
		TopK:         3,
		Scope:        domain.ScopeSystem, // ADR-0034: workspace enrichment is a kernel read
	})

	enrichment := domain.LTMEnrichment{Facts: facts, Negatives: negatives}
	if w.PolicyProvider != nil {
		ep := <-episodicCh
		enrichment.Episodes = ep.hits
	}
	// ADR-0049 D11 (Issue 013): the precedent lane — give the planner foresight by
	// surfacing prior TRANSITIONS (similar past situations + their outcome + action path),
	// failure-weighted and similarity-gated, for the LLM to reason over before it commits.
	enrichment.Precedents = w.retrievePrecedentLane(ctx, taskQuery)
	return enrichment, nil
}

// retrievePrecedentLane finds scenes similar to the situation being planned and resolves
// them into failure-weighted transitions. Similarity-gated by RetrievalFloor: below it,
// no scene returns → no precedent (never a fabricated analogy). ADR-0049 D11 (Issue 013).
func (w *WorkspaceStageImpl) retrievePrecedentLane(ctx context.Context, query string) []domain.Precedent {
	if w.Store == nil || w.Embedder == nil {
		return nil
	}
	vec, err := w.Embedder.Embed(ctx, query)
	if err != nil {
		return nil
	}
	scenes, err := w.Store.Search(ctx, vec, domain.SearchOptions{
		DocumentType:   domain.DocTypeMnemonicScene,
		TopK:           5,
		RetrievalFloor: w.RetrievalFloor, // the similarity gate
		Scope:          domain.ScopeSystem,
	})
	if err != nil || len(scenes) == 0 {
		return nil
	}
	return retrievePrecedents(ctx, w.Store, scenes)
}

// PrimeForExecution retrieves cross-session FACT documents for the DAGExecutor.
func (w *WorkspaceStageImpl) PrimeForExecution(ctx context.Context, plan *domain.ExecutionPlan, initialContext map[string]string) (map[string]string, error) {
	query := plan.Subject
	if query == "" && len(plan.Steps) > 0 {
		query = plan.Steps[0].Query
	}
	results, err := w.enrich(ctx, query, w.ExecutionSlots, false, "execution")
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
		val := r.Document.Text
		if tag, ok := r.Document.Metadata["conflict_tag"].(string); ok {
			val = tag + " " + val
		}
		m[key] = val
	}
	return m
}

func (w *WorkspaceStageImpl) enrich(ctx context.Context, query string, slots int, applyContradiction bool, caller string) ([]domain.SearchResult, error) {
	// 1. Embed raw query.
	rawVec, err := w.Embedder.Embed(ctx, query)
	if err != nil {
		slog.Warn("WorkspaceStage: embed failed", "caller", caller, "err", err)
		return nil, nil
	}

	// 2. Launch SCENE query concurrently (ADR-0016 §4).
	var sceneResult *domain.SearchResult
	var sceneWg sync.WaitGroup
	sceneWg.Add(1)
	go func() {
		defer sceneWg.Done()
		results, err := w.Store.Search(ctx, rawVec, domain.SearchOptions{
			DocumentType: domain.DocTypeMnemonicScene,
			TopK:         1,
			Scope:        domain.ScopeSystem, // ADR-0034: workspace enrichment is a kernel read
		})
		if err != nil || len(results) == 0 {
			return
		}
		sceneResult = &results[0]
	}()

	// 3. While SCENE query runs, do independent work here.
	// (In the Planner path, system-prompt assembly would happen concurrently here.)

	sceneWg.Wait()
	sceneHits := 0
	queryVec := rawVec

	if sceneResult != nil && len(sceneResult.Document.Embedding.Vector) > 0 {
		sceneHits = 1
		primed := blendEmbeddings(rawVec, sceneResult.Document.Embedding.Vector)

		// Drift guard: revert to raw if primed diverges too far.
		if w.DriftGuard {
			sim := cosineSimilarity(rawVec, primed)
			if sim < w.DriftThreshold {
				slog.Info("WorkspaceStage: SCENE drift detected, reverting to raw embedding",
					"workspace_scene_drift_detected", true, "similarity", fmt.Sprintf("%.3f", sim), "caller", caller)
			} else {
				queryVec = primed
			}
		} else {
			queryVec = primed
		}
	}

	// 4. FACT query with (possibly primed) embedding.
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
			"workspace_cold_start", true, "scene_hits", sceneHits, "fact_hits", 0, "caller", caller)
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

	// 7. Contradiction guard (PrimeForPlanning only).
	if applyContradiction {
		w.applyContradictionGuard(ctx, results)
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
			SceneHits:       sceneHits,
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
		"caller", caller, "workspace_cold_start", false, "scene_hits", sceneHits, "fact_hits", len(results))
	return results, nil
}

// isToolOutputDoc reports whether a document is a recorded tool output (an event
// breadcrumb), not a knowledge fact — excluded from the contradiction guard.
func isToolOutputDoc(d domain.Document) bool {
	if d.DocumentType == domain.DocTypeMnemonicAction {
		return true
	}
	src, _ := d.Metadata["source_agent"].(string)
	return src == "ToolOutput"
}

func (w *WorkspaceStageImpl) applyContradictionGuard(ctx context.Context, results []domain.SearchResult) {
	if len(results) < 2 || w.Generator == nil {
		return
	}

	for i := 0; i < len(results); i++ {
		for j := i + 1; j < len(results); j++ {
			a := results[i].Document
			b := results[j].Document
			// A tool-output record is an EVENT breadcrumb (e.g. "wrote file X", "appended
			// 19 B"), not a knowledge claim — two of them can't meaningfully contradict,
			// yet near-identical JSON blobs always clear the similarity gate and burn an
			// LLM call per pair every planning round. Skip them (the real fix is to stop
			// misrouting side-effecting tool outputs into the fact lane).
			if isToolOutputDoc(a) || isToolOutputDoc(b) {
				continue
			}
			if len(a.Embedding.Vector) == 0 || len(b.Embedding.Vector) == 0 {
				continue
			}
			sim := cosineSimilarity(a.Embedding.Vector, b.Embedding.Vector)
			if sim > 0.85 && w.Generator != nil {
				prompt := domain.PromptBuild(
					domain.PromptSystem(
						"You are a semantic contradiction detector.",
						"Respond with AGREE if the two statements are consistent, CONFLICT if they contradict each other.",
					),
					domain.PromptContext(fmt.Sprintf("A: %s\nB: %s", a.Text, b.Text)),
					domain.PromptTask("Do these two statements agree or conflict?"),
					domain.PromptOutputSchemaEnum("AGREE", "CONFLICT"),
				)
				resp, err := w.Generator.Generate(ctx, prompt)
				if err != nil {
					continue
				}
				if resp == "CONFLICT" {
					tag := fmt.Sprintf("[CONFLICT: %s vs %s]", a.ID, b.ID)
					results[i].Document.Metadata["conflict_tag"] = tag
					results[j].Document.Metadata["conflict_tag"] = tag
				}
			}
		}
	}
}

// blendEmbeddings creates a simple mean of two embeddings.
func blendEmbeddings(a, b []float32) []float32 {
	if len(a) == 0 || len(b) == 0 {
		return a
	}
	blended := make([]float32, len(a))
	for i := range a {
		blended[i] = (a[i] + b[i]) / 2
	}
	return blended
}
