package domain

// MemoryPressureScavenger publishes a MemoryPressureEvent when configurable
// thresholds are exceeded. No background ticker is used — it is called
// explicitly when documents are committed (check-then-publish). ADR-0030.
type MemoryPressureScavenger struct {
	// MaxOrphanedDocuments triggers MemoryPressureEvent when exceeded.
	// Zero disables orphaned-document threshold checking.
	MaxOrphanedDocuments int

	// MaxIndexSizeBytes triggers MemoryPressureEvent when exceeded.
	// Zero disables index-size threshold checking.
	MaxIndexSizeBytes int64

	// Bus is the internal event bus for publishing MemoryPressureEvent.
	// nil disables event publication.
	Bus EventBus
}

// OnDocumentCommitted evaluates the given metrics against configured thresholds
// and publishes a MemoryPressureEvent when any threshold is exceeded.
// It is called at document commit time — no background polling required.
func (s *MemoryPressureScavenger) OnDocumentCommitted(metrics MemoryMetrics) {
	if s.Bus == nil {
		return
	}
	if s.MaxOrphanedDocuments > 0 && metrics.OrphanedDocuments > s.MaxOrphanedDocuments {
		_ = s.Bus.Publish(MemoryPressureEvent{
			TotalDocuments: metrics.TotalDocuments,
			IndexSizeBytes: metrics.IndexSizeBytes,
			Trigger:        string(ConsolidationTriggerPressure),
		})
		return
	}
	if s.MaxIndexSizeBytes > 0 && metrics.IndexSizeBytes > s.MaxIndexSizeBytes {
		_ = s.Bus.Publish(MemoryPressureEvent{
			TotalDocuments: metrics.TotalDocuments,
			IndexSizeBytes: metrics.IndexSizeBytes,
			Trigger:        string(ConsolidationTriggerPressure),
		})
	}
}
