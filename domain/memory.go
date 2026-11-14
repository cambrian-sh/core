package domain

// ProceduralMemory (the ADR-0027 Hippocampus plan-template store) was deleted
// 2026-11-14 (owner decision, third round; ENG-1301): its writer had no production
// caller since 2026-07-18. Learned routines are ADR-0139 D13 skills.

// MemoryAgent is the full memory curation interface used by components that need
// both read and write access to the episodic memory layer.
// ADR-0025: MemoryFetcher (FetchContext) removed — no longer part of the planning path.
// The Watcher's signal enrichment reads through QueryService.RecallFor as the
// signal's principal (ENG-1339); FetchContext and domain.MemoryFetcher are deleted.
//
// The MemoryIngester seam (IngestSync / ProcessAndStoreAsync) was REMOVED: it was the
// per-item LLM-importance write path, superseded by the ADR-0015 Tier-1/Tier-2 drain —
// itself deleted on 2026-11-05 with RecordExecution (decisions/scenes-cancelled.md).
//
// IngestNegativeEdge and PoisonMemory were removed from this port on 2026-11-14
// (owner decision, ENG-1336): neither had a production caller. Failure precedents are
// written by MemoryRecorder.RecordPlanOutcome.
type MemoryAgent interface {
	MemoryRecorder
}
