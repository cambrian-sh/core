package domain

import "context"

// MemorySearcher is the narrow read interface for the memory stack's query service.
// Callers (e.g. substrate Server) inject this instead of a full VectorStore so that
// ACL filtering and embedding stay inside the memory stack.
type MemorySearcher interface {
	Search(ctx context.Context, query, callerID string) ([]SearchResult, error)
	// SearchActions is the "what did I do" lane (ADR-0049 D4) — retrieves action
	// records, kept separate from fact recall so they don't re-bloat fact grounding.
	SearchActions(ctx context.Context, query, callerID string) ([]SearchResult, error)
	// SearchScenes is the situational lane (ADR-0049 D7) — finds scenes whose
	// abstracted projection matches the query situation ("have I been here before?").
	SearchScenes(ctx context.Context, query, callerID string) ([]SearchResult, error)
	// SearchEntities is the EXACT-lookup lane (ADR-0049 D8/Issue 012) — resolves a
	// canonical kind:id to one entity's reconstructed current state ("what is true of
	// that file/api now?"), not a semantic search.
	SearchEntities(ctx context.Context, query, callerID string) ([]SearchResult, error)
	// SearchPrecedents is the world-model lane (ADR-0049 D11/Issue 014) — finds prior
	// transitions (situation → outcome + action path), failure-weighted and similarity-
	// gated, so the agent can anticipate the consequence of its next action.
	SearchPrecedents(ctx context.Context, query, callerID string) ([]SearchResult, error)
}
