package domain

import "time"

// ConsolidationTrigger identifies what triggered a consolidation run.
type ConsolidationTrigger string

const (
	ConsolidationTriggerPressure ConsolidationTrigger = "memory_pressure"
	ConsolidationTriggerExplicit ConsolidationTrigger = "explicit_request"
	ConsolidationTriggerSession  ConsolidationTrigger = "session_completion"
)

// MemoryMetrics carries observable state of the pgvector document store.
type MemoryMetrics struct {
	TotalDocuments      int
	IndexSizeBytes      int64
	OrphanedDocuments   int
	StaleDocuments      int
	AvgQueryLatencyMs   float64
	LastConsolidationAt time.Time
}

// ConsolidationScope constrains which documents a consolidation run touches.
type ConsolidationScope struct {
	SessionID     string
	Since         time.Time
	MaxDocuments  int
	DocumentTypes []string
}

// LazyConsolidator runs memory consolidation only when triggered. ADR-0030.
type LazyConsolidator interface {
	// ShouldConsolidate returns true when the given trigger and metrics
	// indicate consolidation should run.
	ShouldConsolidate(trigger ConsolidationTrigger, metrics MemoryMetrics) (bool, ConsolidationTrigger)

	// Consolidate runs consolidation for the given scope (session-local or global).
	Consolidate(scope ConsolidationScope) error
}

// ThresholdConsolidator is a LazyConsolidator that fires when configurable
// document-count or index-size thresholds are exceeded, or on explicit request.
type ThresholdConsolidator struct {
	maxDocCount   int
	maxIndexBytes int64
}

// NewThresholdConsolidator constructs a ThresholdConsolidator.
// Zero values disable the corresponding threshold.
func NewThresholdConsolidator(maxDocCount int, maxIndexBytes int64) *ThresholdConsolidator {
	return &ThresholdConsolidator{
		maxDocCount:   maxDocCount,
		maxIndexBytes: maxIndexBytes,
	}
}

// ShouldConsolidate implements LazyConsolidator.
func (c *ThresholdConsolidator) ShouldConsolidate(trigger ConsolidationTrigger, metrics MemoryMetrics) (bool, ConsolidationTrigger) {
	if trigger == ConsolidationTriggerExplicit {
		return true, ConsolidationTriggerExplicit
	}
	if c.maxDocCount > 0 && metrics.TotalDocuments >= c.maxDocCount {
		return true, trigger
	}
	if c.maxIndexBytes > 0 && metrics.IndexSizeBytes > c.maxIndexBytes {
		return true, trigger
	}
	return false, ""
}

// Consolidate is a no-op stub. Real consolidation is delegated to ConsolidatorAgent
// via MemoryLifecycleManager. ADR-0030.
func (c *ThresholdConsolidator) Consolidate(_ ConsolidationScope) error { return nil }
