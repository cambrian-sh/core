package domain

import "context"

// MemorySearcher is the narrow read interface for the memory stack's query service.
// Callers (e.g. substrate Server) inject this instead of a full VectorStore so that
// ACL filtering and embedding stay inside the memory stack.
type MemorySearcher interface {
	Search(ctx context.Context, query, callerID string) ([]SearchResult, error)
	// SearchActions (the ADR-0049 D4 "what did I do" lane over mnemonic_action
	// records) was removed with ENG-1645: nothing writes those records since ENG-1595.
	// SearchScenes (ADR-0049 D7) and SearchPrecedents (D11) were removed 2026-11-05
	// with scenes (cambrian-knowledge/decisions/scenes-cancelled.md).
	// SearchProcedures is the ADR-0094 D5 procedural lane — induced routines for the
	// situation being planned. Advisory: a procedure is planner INPUT, never authority.
	SearchProcedures(ctx context.Context, query, callerID string) ([]SearchResult, error)
}
