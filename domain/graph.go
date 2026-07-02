package domain

import (
	"context"
	"time"
)

// EdgeType identifies the semantic relationship between two documents.
// ADR-0017: typed causal edges encode relationships that cosine distance cannot infer.
// ADR-0052: free-form LLM-extracted relations use EdgeExtracted with the verb
// phrase carried in DocumentEdge.Label. The recall path does not branch on
// EdgeType; the spreading engine uses edge.Weight directly.
type EdgeType string

const (
	EdgeCloses      EdgeType = "closes"       // PR/commit closes a ticket or issue
	EdgeSpecifies   EdgeType = "specifies"    // design doc specifies an implementation
	EdgeContradicts EdgeType = "contradicts"  // fact contradicts another fact
	EdgeDiscussedIn EdgeType = "discussed_in" // artifact referenced in a discussion thread
	EdgeFollows     EdgeType = "follows"      // ADR-0049 D10: a step's record follows its dependency step's record
	EdgeCoActivated EdgeType = "co_activated" // ADR-0049 D10: Hebbian — memories co-retrieved together wire together
	EdgeEngaged     EdgeType = "engaged"      // ADR-0049: a scene engaged an entity (world-model structure; both endpoints persisted, FK-safe)
	EdgeExtracted   EdgeType = "extracted"    // ADR-0052: LLM-extracted entity/relation edge; free-form label in DocumentEdge.Label
)

// DocumentEdge represents a typed, weighted causal link between two LTM documents.
// Label carries a free-form verb phrase for EdgeExtracted edges (e.g.
// "researched", "is_friend_of", "caused"); empty for typed edges.
type DocumentEdge struct {
	SourceID  string    `json:"source_id"`
	TargetID  string    `json:"target_id"`
	EdgeType  EdgeType  `json:"edge_type"`
	Weight    float32   `json:"weight"`
	Label     string    `json:"label,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// GraphStore manages the document_edges graph layer.
// Separate from VectorStore — graph operations query a different table.
type GraphStore interface {
	SaveEdge(ctx context.Context, edge DocumentEdge) error
	GetAdjacentEdges(ctx context.Context, docIDs []string) ([]DocumentEdge, error)
	UpdateEdgeWeight(ctx context.Context, sourceID, targetID string, edgeType EdgeType, newWeight float32) error
}

// GraphNodeExpansion represents a document discovered via BFS spreading activation.
type GraphNodeExpansion struct {
	Document         Document
	ActivationEnergy float64 // accumulated activation from spreading + base cosine
	Depth            int     // BFS hop count from nearest seed node
}
