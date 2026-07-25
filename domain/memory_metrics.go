package domain

import "time"

// ConsolidationTrigger identifies what triggered a consolidation run.
//
// The consolidation MACHINERY (LazyConsolidator/ThresholdConsolidator, MemoryLifecycleManager,
// ConsolidatorAgent, EpisodicExtractor) has been removed: none of it was wired, and the
// pipeline was gated on a session state nothing produced. What survives here is what live
// code still uses — the trigger vocabulary and the store metrics — because MemoryPressure is
// still a real observability signal on the operator feed.
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
