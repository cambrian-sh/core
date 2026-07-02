package domain

type AgentRuntime string

const (
	RuntimePython AgentRuntime = "python"
	RuntimeBinary AgentRuntime = "binary"
	RuntimeWasm   AgentRuntime = "wasm"
	RuntimeA2A    AgentRuntime = "a2a"
)

type AgentTrait string

const (
	TraitCognitive AgentTrait = "" // zero value — full Gatekeeper pipeline
	// Deprecated: TraitTool (the DeterministicAgent / static-bidder model) is
	// SUPERSEDED by the kernel-owned tool registry (ADR-0039 A1.3). Deterministic
	// capabilities are now kernel tools invoked via ExecuteTool, not auction-bidding
	// cells. The behavioral static bid (Confidence=1.0 bypass) was already removed
	// in ADR-0023; this constant is retained only for backward-compatible manifest
	// parsing and will be removed once no manifests declare "trait":"tool".
	TraitTool   AgentTrait = "tool"   // deprecated — see above
	TraitModel  AgentTrait = "model"  // LLM inference provider; bypasses Interview, Merit-tracked with cost penalty
	TraitDaemon AgentTrait = "daemon" // autonomous background process; emits signals via SignalStream. ADR-0033.
)

// systemAgentIDs are privileged kernel-invoked organs (ADR-0051): the kernel calls them
// DIRECTLY (e.g. the pre-plan Scout via Auctioneer.CallAgent), never through the auction.
// They are therefore (a) verified by default — they bypass the Gatekeeper interview, and
// (b) excluded from auction/EFE candidate selection — they are never assigned a user task.
// A deterministic system exception, NOT task-to-agent routing, so the Zero-Hardcode Rule is
// unaffected (the same class as the shell/Omurilik/scope exceptions).
var systemAgentIDs = map[string]bool{
	"scout_agent": true,
	// kg_extractor_agent (ADR-0053 D2 revised): the deterministic, NO-LLM tiered
	// triplet extractor (metadata + spacy_patterns). The kernel hands it a chunk
	// batch and gets triplets back; it is invoked DIRECTLY on the ingest path,
	// never auctioned/interviewed — the same privileged-organ class as the Scout.
	"kg_extractor_agent": true,
	// reranker_agent (ADR-0054 Stage B): the warm bge cross-encoder relevance
	// oracle. The kernel hands it {query, documents} on the recall path and gets
	// one relevance score per document back; invoked DIRECTLY via the Auctioneer,
	// never auctioned/interviewed — same privileged-organ class as the Scout and
	// the kg_extractor. DeterministicAgent: a scoring model, no generative think().
	"reranker_agent": true,
	// operator_agent (ADR-0059): the operator's privileged answer path. The
	// operator plane sends chat messages to the kernel via OperatorConsole.
	// SendMessage; that path must bypass the Router/auction/Plan because the
	// operator IS the principal (a benchmark is a first-class operator
	// workload, not a user-task agent). The kernel invokes this organ directly
	// via the LLM broker: recall + LLM call, same shape as the system organs
	// above. Never auctioned/interviewed.
	"operator_agent": true,
}

// IsSystemAgent reports whether agentID is a privileged kernel organ that bypasses the
// interview (verified by default) and is ignored by the Gatekeeper / auction / EFE.
func IsSystemAgent(agentID string) bool {
	return systemAgentIDs[agentID]
}

type AgentDefinition struct {
	ID              string       `json:"ID"`
	Name            string       `json:"Name"`
	Description     string       `json:"Description"` // natural-language description for the LLM
	Runtime         AgentRuntime `json:"Runtime"`
	ExecPath        string       `json:"ExecPath"`
	Dir             string       `json:"Dir"` // working directory (CWD)
	A2AEndpoint     string       `json:"a2a_endpoint,omitempty"`
	SourceHash      string       `json:"source_hash,omitempty"`
	ManifestVersion string       `json:"manifest_version,omitempty"`
	Provisional     bool         `json:"provisional,omitempty"`
	Trait           AgentTrait   `json:"trait,omitempty"`
	Capabilities    []string     `json:"capabilities,omitempty"`
	System bool `json:"system,omitempty"`
	// ScopeProfile is the agent's intrinsic genotype-level access boundary
	// (ADR-0034 D9). It is set by the operator at registration, NOT self-declared.
	// AUTHORITATIVE storage is the PostgreSQL agent_scopes table resolved via
	// ScopeResolver (BBolt deliberately does not hold scope — R1); this in-memory
	// field carries it when an AgentDefinition is assembled with its scope.
	ScopeProfile ScopeConfig `json:"scope_profile,omitempty"`
	// DefaultWriteTags is the operator-configured classification stamped on this
	// agent's writes (ADR-0035 C2). Distinct from ScopeProfile (the READ predicate):
	// this is the flat WRITE classification. The agent cannot choose its own tags —
	// it may only narrow within this set. Authoritative store is PostgreSQL
	// agent_scopes (resolved via ScopeResolver), like ScopeProfile.
	DefaultWriteTags []string `json:"default_write_tags,omitempty"`
}
