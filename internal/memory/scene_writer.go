package memory

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// PgSceneWriter writes shallow DocTypeMnemonicScene documents to pgvector after
// each successful DAG step, and a specifies edge to the prior step's scene.
// ADR-0025: one instance per plan execution (tracks lastSceneID for edge chain).
type PgSceneWriter struct {
	Store      domain.VectorStore
	Embedder   domain.Embedder
	GraphStore domain.GraphStore // may be nil; nil disables specifies edges

	mu          sync.Mutex
	lastSceneID string
}

// WriteScene implements domain.SceneWriter.
// It persists a shallow scene document and writes a specifies edge to the prior scene.
func (sw *PgSceneWriter) WriteScene(ctx context.Context, result domain.StepResult) (string, error) {
	// Shallow content: step query is carried in Output prefix by DAGExecutor.
	summary := result.Output
	if len(summary) > 200 {
		summary = summary[:200]
	}
	text := fmt.Sprintf("step_%d: %s", result.Index, summary)

	vec, err := sw.Embedder.Embed(ctx, text)
	if err != nil {
		slog.Warn("PgSceneWriter: embed failed", "step", result.Index, "err", err)
		return "", err
	}

	sceneID := fmt.Sprintf("scene-%d-%d", result.Index, time.Now().UnixNano())
	doc := &domain.Document{
		ID:                 sceneID,
		DocumentType:       domain.DocTypeMnemonicScene,
		Text:               text,
		Embedding:          domain.Embedding{Vector: vec},
		ActivationStrength: 0.1,
		Metadata: map[string]interface{}{
			"step_index": result.Index,
			"stored_at":  time.Now().Format(time.RFC3339),
		},
	}
	if err := sw.Store.Save(ctx, doc); err != nil {
		slog.Warn("PgSceneWriter: save failed", "step", result.Index, "err", err)
		return "", err
	}

	// Write specifies edge: this scene → prior scene (sequential dependency).
	sw.mu.Lock()
	prior := sw.lastSceneID
	sw.lastSceneID = sceneID
	sw.mu.Unlock()

	if prior != "" && sw.GraphStore != nil {
		if err := sw.GraphStore.SaveEdge(ctx, domain.DocumentEdge{
			SourceID:  sceneID,
			TargetID:  prior,
			EdgeType:  domain.EdgeSpecifies,
			Weight:    0.8,
			CreatedAt: time.Now(),
		}); err != nil {
			slog.Warn("PgSceneWriter: specifies edge failed", "scene", sceneID, "prior", prior, "err", err)
		}
	}

	return sceneID, nil
}

// GraphStoreEdgeWriter adapts domain.GraphStore.SaveEdge to the domain.EdgeWriter
// interface used by the Tier-2 batch scorer for discussed_in edges. ADR-0025.
type GraphStoreEdgeWriter struct {
	GraphStore domain.GraphStore
}

// WriteEdge implements domain.EdgeWriter.
func (ew *GraphStoreEdgeWriter) WriteEdge(ctx context.Context, sourceID, targetID, edgeType string, weight float64) error {
	return ew.GraphStore.SaveEdge(ctx, domain.DocumentEdge{
		SourceID:  sourceID,
		TargetID:  targetID,
		EdgeType:  domain.EdgeType(edgeType),
		Weight:    float32(weight),
		CreatedAt: time.Now(),
	})
}
