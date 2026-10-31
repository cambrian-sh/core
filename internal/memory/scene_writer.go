package memory

import (
	"context"
	"time"

	"github.com/cambrian-sh/core/domain"
)

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
