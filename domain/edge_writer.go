package domain

import "context"

// EdgeWriter writes a typed directed edge between two documents in the graph store.
// ADR-0025: nil = no edge written (zero behaviour change for existing callers).
type EdgeWriter interface {
	WriteEdge(ctx context.Context, sourceID, targetID, edgeType string, weight float64) error
}
