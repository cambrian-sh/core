package domain

type AgentRuntime string

const (
	RuntimePython AgentRuntime = "python"
	RuntimeBinary AgentRuntime = "binary"
	RuntimeWasm   AgentRuntime = "wasm"
	RuntimeA2A    AgentRuntime = "a2a"
	// RuntimeBun / RuntimeNode are the first-class JS agent runtimes (ADR-0125):
	// the kernel spawns the configured interpreter (metabolism.bun_executable /
	// metabolism.node_executable, $PATH fallback) with the same argv contract as
	// Python agents. Kernel-side support only — the TS SDK is future work.
	RuntimeBun  AgentRuntime = "bun"
	RuntimeNode AgentRuntime = "node"
)

// IsJSRuntime reports whether r is one of the V8/JSC-based runtimes (ADR-0125).
// Used where JS engines need distinct handling (e.g. the Linux RLIMIT_AS memory
// cap, which their large virtual reservations would trip spuriously).
func IsJSRuntime(r AgentRuntime) bool {
	return r == RuntimeBun || r == RuntimeNode
}

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
// DIRECTLY (e.g. the kg_extractor via the agent transport's CallAgent), never through selection.
// They are therefore (a) verified by default — they bypass the Gatekeeper interview, and
// (b) excluded from auction/EFE candidate selection — they are never assigned a user task.
// A deterministic system exception, NOT task-to-agent routing, so the Zero-Hardcode Rule is
// unaffected (the same class as the shell/Omurilik/scope exceptions).
var systemAgentIDs = map[string]bool{
	// kg_extractor_agent (ADR-0053 D2 revised): the deterministic, NO-LLM tiered
	// triplet extractor (metadata + spacy_patterns). The kernel hands it a chunk
	// batch and gets triplets back; it is invoked DIRECTLY on the ingest path,
	// never auctioned/interviewed — the privileged-organ class.
	"kg_extractor_agent": true,
	// reranker_agent (ADR-0054 Stage B): the warm bge cross-encoder relevance
	// oracle. The kernel hands it {query, documents} on the recall path and gets
	// one relevance score per document back; invoked DIRECTLY via the agent transport,
	// never auctioned/interviewed — same privileged-organ class as the kg_extractor and
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
	// retrieval_agent (AGENTIC_RETRIEVAL_SPEC §2.1): the LLM steps of the agentic
	// retrieval loop (plan_step / decide_continue / synthesize). The kernel calls
	// it DIRECTLY via the agent transport on the recall path with a managed LLM session
	// (RetrievalDispatcher), never auctioned/interviewed — same privileged-organ
	// class as the other system organs. CognitiveAgent: makes managed generate() calls.
	"retrieval_agent": true,
	// chat_agent (ADR-0084): the conversational front desk. The kernel's chat worker
	// pool dispatches it DIRECTLY (CallDaemon) to own one conversation turn; it never
	// executes planner steps and has no work tools — it only converses and delegates
	// whole tasks to the planner. It must be excluded from the auction/interview for
	// two reasons: (a) it is incapable of doing task work, so routing a step to it dead-
	// loops on empty memory queries; (b) letting the planner route back to it creates a
	// chat→planner→chat recursion. Same privileged-organ class as the other organs — the
	// configured ChatPoolAgentID defaults to this ID.
	"chat_agent": true,
}

// IsSystemAgent reports whether agentID is a privileged kernel organ that bypasses the
// interview (verified by default) and is ignored by the Gatekeeper / auction / EFE.
func IsSystemAgent(agentID string) bool {
	return systemAgentIDs[agentID]
}

// IsSystemDefinition reports whether a DEFINITION is privileged, by either route.
//
// `systemAgentIDs` above can only ever name organs that live in this repository,
// and a compiled-in plugin's agent is just as kernel-invoked: the source
// discovery agent is called directly by the ingress studio's discovery plane and
// is never assigned a user task. Naming it in the map above would put a premium
// agent ID in OSS core, where it does not exist — so the second route is
// `Registry.AddSystemAgent`, which stamps `System` as an explicit auditable
// grant only a trusted compiled-in plugin can reach.
//
// Prefer this over IsSystemAgent wherever an AgentDefinition is in hand. Callers
// holding only an ID keep the map lookup, which is why both exist.
func IsSystemDefinition(a AgentDefinition) bool {
	return a.System || systemAgentIDs[a.ID]
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
	System          bool         `json:"system,omitempty"`
	// ClassificationTags describe what this agent IS, so a policy can permit or
	// deny INVOKING it (ADR-0085 D2). They are deliberately NOT the agent's own
	// read boundary: that is the agent's access profile, which now lives entirely
	// in the policy plugin (the former ScopeProfile / DefaultWriteTags fields were
	// carried here but read by nothing, and their authoritative home was always
	// the agent_scopes table). Operator-set at registration, never self-declared.
	ClassificationTags []string `json:"classification_tags,omitempty"`
}

// AuthzRef identifies an agent to the decision point (Taggable).
func (a AgentDefinition) AuthzRef() ResourceRef { return ResourceRef{Kind: KindAgent, ID: a.ID} }

// AuthzTags presents the agent's invocation classification (Taggable).
func (a AgentDefinition) AuthzTags() []string { return a.ClassificationTags }
