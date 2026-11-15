package memory

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// SpreadingEngine performs bounded BFS over the document_edges graph.
// ADR-0017: propagates activation energy from pgvector seed hits to graph-discovered nodes.
// ADR-0053 D2: edge weight is read directly from DocumentEdge.Weight (LLM confidence
// for extracted edges; Hebbian co-retrieval strength for co_activated edges). The
// per-type weight map (contradicts/specifies/closes/discussed_in) is removed —
// the recall path does not branch on EdgeType.
type SpreadingEngine struct {
	GraphStore  domain.GraphStore
	VectorStore domain.VectorReader
	DecayFactor float64 // default 0.75
	MaxDepth    int     // default 3
	EnergyFloor float64 // default 0.15
	// HebbianDecayPerDay applies lazy decay to a co_activated edge's STORED weight
	// when the spreader traverses it (ADR-0049 D10 — decay-on-spread-read), so a
	// stale Hebbian link contributes less over time without any background sweep.
	// 0 (or ≥1) disables decay. Compute-only — the stored weight is not rewritten.
	HebbianDecayPerDay float64
	TraversalLogger    domain.TraversalLogger // ADR-0021: may be nil
}

// NewSpreadingEngine creates a SpreadingEngine with optional config overrides.
func NewSpreadingEngine(gs domain.GraphStore, vs domain.VectorReader, decay float64, maxDepth int, energyFloor float64) *SpreadingEngine {
	if decay <= 0 {
		decay = 0.75
	}
	if maxDepth <= 0 {
		maxDepth = 3
	}
	if energyFloor <= 0 {
		energyFloor = 0.15
	}
	return &SpreadingEngine{
		GraphStore:  gs,
		VectorStore: vs,
		DecayFactor: decay,
		MaxDepth:    maxDepth,
		EnergyFloor: energyFloor,
	}
}

// Spread performs BFS from seed hits and returns expanded + original results.
// Formula: A_j = (BaseCosine_j + Σ(A_i · w_ij)) · d^depth · activation_strength_j
func (s *SpreadingEngine) Spread(ctx context.Context, seeds []domain.SearchResult) []domain.GraphNodeExpansion {
	if len(seeds) == 0 {
		return nil
	}

	// Phase 1: build initial activation map from seeds.
	energies := make(map[string]domain.GraphNodeExpansion)
	// order records first-insertion order so the result is deterministic: ranging
	// over the energies map returned ties in random order run to run (ENG-1683).
	order := make([]string, 0, len(seeds))
	seedIDs := make([]string, 0, len(seeds))
	// Track IDs that contributed edges: if none had edges, log graph miss.
	edgeCoverage := 0
	totalSeeds := len(seeds)

	for _, seed := range seeds {
		seedIDs = append(seedIDs, seed.Document.ID)
		if _, dup := energies[seed.Document.ID]; !dup {
			order = append(order, seed.Document.ID)
		}
		energies[seed.Document.ID] = domain.GraphNodeExpansion{
			Document:         seed.Document,
			ActivationEnergy: seed.Score, // initial cosine score as base energy
			Depth:            0,
		}
	}

	// Phase 2: BFS levels.
	queue := make([]string, len(seedIDs))
	copy(queue, seedIDs)
	dequeued := make(map[string]bool)
	for _, id := range seedIDs {
		dequeued[id] = true
	}

	for level := 0; level < s.MaxDepth && len(queue) > 0; level++ {
		// Batch query: all edges for current queue level.
		edges, err := s.GraphStore.GetAdjacentEdges(ctx, queue)
		if err != nil {
			slog.Warn("SpreadingEngine: GetAdjacentEdges failed", "err", err, "level", level)
			break
		}

		if len(edges) > 0 && level == 0 {
			edgeCoverage = len(seedIDs)
		}
		if len(edges) == 0 && level == 0 {
			slog.Debug("SpreadingEngine: no edges found for seed batch",
				"bfs_graph_miss", true, "source_count", totalSeeds)
		}

		var nextQueue []string

		// ENG-83: fetch every target this level can reach in ONE enforced GetBatch
		// (scope rides ctx, ADR-0095 D9) instead of one GetByID per edge. Only targets
		// whose transferred energy clears the floor are fetched, as before.
		docs := s.fetchTargets(ctx, edges, energies, dequeued)

		for _, edge := range edges {
			// Look up the source node's accumulated energy.
			src, ok := energies[edge.SourceID]
			if !ok {
				continue
			}
			if dequeued[edge.TargetID] {
				continue // cycle prevention
			}

			// ADR-0053 D2: weight is read directly from the edge. The only special
			// case is the Hebbian co_activated path (ADR-0049 D10), which applies
			// decay-on-spread-read to its STORED weight before propagating.
			weight := float64(edge.Weight)
			if edge.EdgeType == domain.EdgeCoActivated {
				weight = s.coActivatedWeight(edge)
			}
			transferred := src.ActivationEnergy * weight

			// Check if accumulated energy is below floor.
			if transferred < s.EnergyFloor {
				continue
			}

			// The target document (prefetched) gives activation_strength; unreadable or
			// gone => no expansion, as the per-edge read did.
			doc, ok := docs[edge.TargetID]
			if !ok {
				continue
			}

			// Activation formula.
			as := doc.ActivationStrength
			decay := math.Pow(s.DecayFactor, float64(level+1))
			baseCosine := 0.0
			if existing, ok := energies[edge.TargetID]; ok {
				baseCosine = existing.ActivationEnergy
			}

			energy := (baseCosine + transferred) * decay * as
			if energy < s.EnergyFloor {
				continue
			}

			// ADR-0021: Log graph traversal for deferred Thompson Sampling data collection.
			if s.TraversalLogger != nil {
				entry := domain.TraversalLogEntry{
					EntryID:           fmt.Sprintf("trav-%s-%s-%d", edge.SourceID, edge.TargetID, time.Now().UnixNano()),
					SourceID:          edge.SourceID,
					TargetID:          edge.TargetID,
					EdgeType:          string(edge.EdgeType),
					EdgeWeight:        float64(weight),
					TransferredEnergy: transferred,
					Depth:             level + 1,
					Timestamp:         time.Now(),
				}
				if err := s.TraversalLogger.LogTraversal(entry); err != nil {
					slog.Warn("SpreadingEngine: failed to log traversal", "err", err)
				}
			}

			if _, seen := energies[edge.TargetID]; !seen {
				order = append(order, edge.TargetID)
			}
			energies[edge.TargetID] = domain.GraphNodeExpansion{
				Document:         doc,
				ActivationEnergy: energy,
				Depth:            level + 1,
			}
			nextQueue = append(nextQueue, edge.TargetID)
		}

		for _, id := range nextQueue {
			dequeued[id] = true
		}
		queue = nextQueue
	}

	// Compute edge coverage ratio for SPC logging.
	if totalSeeds > 0 {
		coverage := float64(edgeCoverage) / float64(totalSeeds)
		if coverage < 0.5 && coverage > 0 {
			slog.Info("SpreadingEngine: partial graph coverage",
				"bfs_graph_partial", true, "edge_coverage", fmt.Sprintf("%.2f", coverage))
		}
		if coverage == 0 {
			slog.Debug("SpreadingEngine: no graph edges available",
				"bfs_graph_miss", true, "source_count", totalSeeds)
		}
	}

	// Collect all expansions.
	result := make([]domain.GraphNodeExpansion, 0, len(order))
	for _, id := range order {
		result = append(result, energies[id])
	}
	return result
}

// fetchTargets reads, in one GetBatch on the (enforcing) vector store, every edge
// target of this BFS level that the per-edge loop could expand: not already
// dequeued, reachable from a source with energy, and with transferred energy at or
// above the floor (ENG-83). A failed batch read logs and expands nothing this level
// - the per-edge reads it replaces skipped a target on error too (fail open).
func (s *SpreadingEngine) fetchTargets(ctx context.Context, edges []domain.DocumentEdge, energies map[string]domain.GraphNodeExpansion, dequeued map[string]bool) map[string]domain.Document {
	var ids []string
	want := map[string]bool{}
	for _, edge := range edges {
		src, ok := energies[edge.SourceID]
		if !ok || dequeued[edge.TargetID] || want[edge.TargetID] {
			continue
		}
		weight := float64(edge.Weight)
		if edge.EdgeType == domain.EdgeCoActivated {
			weight = s.coActivatedWeight(edge)
		}
		if src.ActivationEnergy*weight < s.EnergyFloor {
			continue
		}
		want[edge.TargetID] = true
		ids = append(ids, edge.TargetID)
	}
	out := make(map[string]domain.Document, len(ids))
	if len(ids) == 0 {
		return out
	}
	docs, err := s.VectorStore.GetBatch(ctx, ids)
	if err != nil {
		slog.Warn("SpreadingEngine: target batch read failed; no expansion this level", "err", err, "targets", len(ids))
		return out
	}
	for _, d := range docs {
		out[d.ID] = d
	}
	return out
}

// coActivatedWeight returns a co_activated edge's STORED weight decayed by its age
// (ADR-0049 D10, decay-on-spread-read). Compute-only — the edge is not rewritten.
func (s *SpreadingEngine) coActivatedWeight(edge domain.DocumentEdge) float64 {
	w := float64(edge.Weight)
	if s.HebbianDecayPerDay > 0 && s.HebbianDecayPerDay < 1 && !edge.CreatedAt.IsZero() {
		if ageDays := time.Since(edge.CreatedAt).Hours() / 24; ageDays > 0 {
			w *= math.Pow(s.HebbianDecayPerDay, ageDays)
		}
	}
	return w
}
