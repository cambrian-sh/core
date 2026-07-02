package domain

import "context"

// CID is a content identifier: SHA-256 of the raw bytes, hex-encoded.
// ADR-0022 Layer 2: CAS (Content-Addressed Store).
type CID string

// ContextRef is a lightweight handle to a piece of content in the ContentStore.
// It is the unit of currency in the Global Workspace (ADR-0022 Layer 4).
//
// Precision semantics:
//   - Precision >= 0: cosine similarity to the step query (pgvector seeds have this)
//   - Precision == -1.0: sentinel — not yet computed (BFS-discovered nodes)
//
// assemble_context() in the Python SDK uses Precision to re-rank; the -1.0 sentinel
// distinguishes "unknown" from "mediocre" (a 0.5 default would be ambiguous).
type ContextRef struct {
	CID        CID
	Type       string   // "step_result", "ltm_doc", "verification", "agent_artifact"
	Labels     []string // searchable tags
	Activation float32  // spreading activation energy (0–1); structural relevance
	Precision  float32  // cosine similarity (0–1), or -1.0 if not yet computed
	Snippet    string   // first ContextRefSnippetChars of UTF-8 content; "" for binary
}

// ContextNode is a fully-resolved piece of content retrieved from the ContentStore.
type ContextNode struct {
	CID          CID
	Type         string
	Data         []byte
	Labels       []string
	Parents      []CID  // provenance edges — NOT part of CID computation
	Snippet      string // inline resilience snippet (see ContextRef.Snippet)
	OwnerSession string // ADR-0048 D4: session that wrote this node; "" = system/ownerless
}

// CanReadContentNode is the ContentStore read-gate (ADR-0048 D4). An ownerless
// node — system/kernel content (tool results, step results) or legacy data — is
// readable by anyone (fail-open). An owned node (an agent's offloaded working
// memory) is readable only by its owning session (fail-closed on mismatch), so
// one agent cannot read another's offload by observing a cid.
func CanReadContentNode(ownerSession, callerSession string) bool {
	return ownerSession == "" || ownerSession == callerSession
}

// CacheInvalidator is notified after every Tier-2 pgvector drain so that
// query → []ContextRef caches can be cleared. ADR-0022 Phase 2B.
// WorkspaceStageImpl implements this interface.
type CacheInvalidator interface {
	InvalidateContextRefCache()
}

// ContentStore is the content-addressed key-value layer. ADR-0022 Phase 1.
// Put/Get/Has/GC form the minimal surface that DAGExecutor needs.
type ContentStore interface {
	// Put stores data and returns its CID. Idempotent: if CID already exists,
	// returns the existing CID without writing. snippet is the pre-computed
	// resilience snippet (caller validates UTF-8 and truncates).
	Put(ctx context.Context, data []byte, nodeType string, labels []string, snippet string) (CID, error)
	// Get retrieves a ContextNode by CID. Returns an error if not found.
	Get(ctx context.Context, cid CID) (*ContextNode, error)
	// Has reports whether the given CID exists.
	Has(ctx context.Context, cid CID) (bool, error)
	// GC evicts all CIDs not present in keep. Must be called via defer in Execute().
	GC(ctx context.Context, keep []CID) error
}
