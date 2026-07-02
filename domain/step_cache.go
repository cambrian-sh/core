package domain

import (
	"context"
	"time"
)

// StepCache is the port for step-level output memoization (ADR-0026).
// Implementations may be swapped (BBolt, Redis, in-memory) without touching DAGExecutor.
type StepCache interface {
	Get(ctx context.Context, key string) (*Handoff, bool, error)
	Put(ctx context.Context, key string, handoff *Handoff, ttl time.Duration) error
}
