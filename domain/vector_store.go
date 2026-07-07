package domain

import (
	"context"
	"time"
)

// DocType constants identify the category of a Document stored in pgvector.
const (
	// DocTypeMemory is RETIRED (ADR-0025). No new documents may use this type.
	// Existing rows decay via apply_ebbinghaus_decay(). All query paths updated to DocTypeMnemonicFact.
	DocTypeMemory             = "memory"
	DocTypeAgentProfile       = "agent_profile"
	DocTypeJudicialRecord     = "judicial_record"
	DocTypeProceduralTemplate = "procedural_template"
	DocTypeNeuralTrace        = "neural_trace"
	DocTypeNegativeEdge       = "negative_edge"
	DocTypeMnemonicFact       = "mnemonic_fact"   // ADR-0015: structured step output (tool response, agent result)
	DocTypeMnemonicAction     = "mnemonic_action" // ADR-0049: a mutation/side-effecting tool call — an EVENT ("what I did"), not knowledge
	DocTypeMnemonicScene      = "mnemonic_scene"  // ADR-0015: masterContext snapshot at step completion time
	DocTypeMnemonicEntity     = "mnemonic_entity" // ADR-0049 D8: a first-class engaged THING (file/dir/api/…), keyed by canonical kind:id
	DocTypeEpisodicMemory     = "episodic_memory" // ADR-0029: session narrative index (goal + decisions)
	DocTypeTool               = "tool"            // ADR-0044: tool descriptor indexed for semantic retrieval
	DocTypeSkill              = "skill"           // ADR-0046: system-skill descriptor indexed for semantic retrieval
	DocTypeDocSection         = "doc_section"     // ADR-0060: a structural section node (chapter/section/subsection) of an ingested document; NOT embedded, excluded from fact recall
)

// SearchOptions carries all optional parameters for a VectorStore.Search call.
type SearchOptions struct {
	DocumentType    string // empty string means no filter (returns all)
	TopK            int
	Filter          string    // Optional: Additional SQL filter
	RetrievalFloor  float64   // ADR-0015: α in floor-multiplier formula; 0 disables re-ranking
	ExplorationRate float64   // ADR-0015: fraction of returned slots reserved for random exploration
	Since           time.Time // ADR-0017: temporal filter; zero = no filter
	// DecayLambda is the temporal decay rate λ (per hour) applied at query time:
	// effective_activation = activation_strength × e^(-λ × age_hours).
	// Zero means no temporal decay (uses raw activation_strength). ADR-0030.
	DecayLambda float64
	// Scope is the effective access boundary applied to this search. It is the
	// explicit, compiler-visible chokepoint parameter (ADR-0034 D5). The
	// ScopedVectorStore decorator seeds it (from ctx or an explicit value) and
	// fails closed when it is nil; the pgvector adapter translates it into the
	// three-set/CNF jsonb containment predicate. ScopeSystem bypasses filtering.
	Scope *EffectiveScope
}

// Embedding represents the vector data and its associated metadata.
type Embedding struct {
	Vector []float32
	Model  string
	Size   int
}

// Document represents an entity mapping to a vector in the database, with metadata.
type Document struct {
	ID           string
	DocumentType string // one of the DocType constants; defaults to DocTypeMemory when empty
	Text         string
	// Summary is a one-line descriptor of Text (ADR-0048 #1 summary column). When
	// set, recall returns THIS as the agent-facing surface (and it is what gets
	// embedded) so a fact is represented by its gist, not its full body; the full
	// content stays in Text and, when offloaded, behind metadata["content_cid"] for
	// drill-down. Empty for short docs where Text is already its own summary.
	Summary              string
	Embedding            Embedding
	Metadata             map[string]interface{}
	AccessCount          int
	ActivationStrength   float64   `json:"activation_strength"`              // ADR-0015: lifecycle metric [0.0, 1.0]
	ScoringPromptVersion string    `json:"scoring_prompt_version,omitempty"` // ADR-0015: hash of scoring prompt template
	CreatedAt            time.Time `json:"created_at"`                       // ADR-0015: for GC predicate
	LastAccessedAt       time.Time
	// 🛡️ REDEMPTION: Concurrency Control
	// Matches the 'version' column in the database.
	Version int `json:"version"`
}

// SearchResult holds the search hit containing the document and its similarity score.
type SearchResult struct {
	Document Document
	Score    float64 // floor-multiplier-adjusted score: cosine × (α + (1-α) × activation_strength)
	RawScore float64 // pre-multiplier cosine similarity: 1 - cosine_distance. Used for MinFactCosine filtering.
	// LexicalScore is the chunk's lexical (full-text) relevance signal in [0,1]
	// from hybrid retrieval's RRF fusion (ADR-0054): the reciprocal of its rank in
	// the lexical list (1.0 = top lexical hit, 0 = not lexically matched). Fed into
	// the Stage-A blend so an exact-token chunk gets credit even at low cosine.
	LexicalScore float64
}

// VectorStore defines the interface for underlying vector database operations.
type VectorStore interface {
	Save(ctx context.Context, doc *Document) error
	SaveBatch(ctx context.Context, docs []*Document) error
	Search(ctx context.Context, vector []float32, opts SearchOptions) ([]SearchResult, error)
	GetByID(ctx context.Context, id string) (*Document, error)
	GetBatch(ctx context.Context, ids []string) ([]Document, error)
	Delete(ctx context.Context, id string) error
	DeleteBatch(ctx context.Context, ids []string) error
	IncrementAccess(ctx context.Context, id string) error
	GetStaleMemories(ctx context.Context, limit int) ([]Document, error)
	// QueryByMetadata returns documents whose metadata JSONB contains all key-value
	// pairs in filter (PostgreSQL @> containment). Results ordered by created_at ASC.
	// limit == 0 returns all matching documents. ADR-0033.
	QueryByMetadata(ctx context.Context, filter map[string]string, limit int) ([]Document, error)
}
