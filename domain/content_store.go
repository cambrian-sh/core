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
	Parents      []CID     // provenance edges — NOT part of CID computation
	Snippet      string    // inline resilience snippet (see ContextRef.Snippet)
	OwnerSession SessionID // ADR-0048 D4: session that wrote this node; "" = system/ownerless
}

// CanReadContentNode is the ContentStore read-gate (ADR-0048 D4). An ownerless
// node — system/kernel content (tool results, step results) or legacy data — is
// readable by anyone (fail-open). An owned node (an agent's offloaded working
// memory) is readable only by its owning session (fail-closed on mismatch), so
// one agent cannot read another's offload by observing a cid.
func CanReadContentNode(ownerSession, callerSession SessionID) bool {
	return ownerSession == "" || ownerSession == callerSession
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
	// GC evicts PLAN-SCOPED scratch nodes (IsPlanScopedNodeType) that are not in
	// keep. Every other node type is durable and never evicted by GC.
	//
	// A store that also implements PlanScratchTracker never evicts, from any GC
	// call, scratch that a still-live plan may need (see PlanScratchTracker); the
	// executor then ends a plan with EndPlan instead of GC. A store that does not
	// track plans evicts every not-kept scratch node, so concurrent plans can
	// evict each other's scratch (T-20261103-a-plan-s-content-store-gc-evicts-a-concurrently).
	//
	// Why type-scoped: ADR-0022 designed this store as a plan's scratch space,
	// but evidence blobs (ADR-0105), source-document bodies (ADR-0060 D8), memory
	// descriptors and tool artifacts were later written into the same store.
	// "Evict everything not kept" then destroyed them after every plan
	// (T-20261103-plan-scoped-contentstore-gc-evicts-evidence).
	GC(ctx context.Context, keep []CID) error
}

// PlanLease identifies one live plan registered with a PlanScratchTracker.
type PlanLease uint64

// PlanScratchTracker is an optional ContentStore capability: it tracks which
// plans are live so that one plan's GC never evicts scratch another live plan
// may still need (T-20261103-a-plan-s-content-store-gc-evicts-a-concurrently).
//
// Semantics: plan-scoped scratch written (or re-written through dedup) after a
// plan's BeginPlan is protected until that plan's EndPlan. EndPlan deregisters
// the plan and evicts every plan-scoped scratch node that no plan still live
// could have written — after the last live plan ends, all scratch is gone.
// Attribution is by write order, not by plan identity, because tool_result
// scratch is written inside an agent's ExecuteTool RPC whose context carries no
// plan id. The protection is therefore conservative: a long plan also shields
// scratch other plans wrote while it ran, until it ends. Durable node types are
// never evicted, as with GC.
//
// Optional (detected by type assertion) rather than part of ContentStore so the
// many ContentStore test fakes need not change; a store without it falls back
// to GC(keep).
type PlanScratchTracker interface {
	// BeginPlan registers a live plan. Call before the plan writes any scratch.
	BeginPlan(ctx context.Context) (PlanLease, error)
	// EndPlan deregisters the plan and collects scratch no live plan needs.
	// Must be deferred so it fires on panic; an unknown lease is a no-op
	// deregistration but still collects.
	EndPlan(ctx context.Context, lease PlanLease) error
}

// Plan-scoped node types: scratch written while a plan executes and collected
// by the plan's GC. Anything else (evidence, source_document, ltm_fact,
// tool_artifact, agent-plane writes of any other type) is durable.
const (
	NodeTypeStepResult = "step_result"
	NodeTypeToolResult = "tool_result"
)

// IsPlanScopedNodeType reports whether a node of this type may be evicted by a
// plan's GC. Unknown types are durable: a type nobody declared scratch must not
// be destroyed by a plan that never wrote it.
func IsPlanScopedNodeType(nodeType string) bool {
	return nodeType == NodeTypeStepResult || nodeType == NodeTypeToolResult
}
