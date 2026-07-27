package domain

import (
	"context"
	"log/slog"
	"sync"
)

// SystemTool is a kernel-owned tool definition (ADR-0039 D1, amended A1). A tool
// is a Python module + manifest auto-discovered into a BBolt registry; this is
// the kernel's in-memory view of one. Authorization is enforced by ToolExecutor
// against the agent's grant BEFORE the handler runs (A1.4); the manifest is not
// a trust input for the resource policy (A1.5).
type SystemTool struct {
	Name        string
	Description string
	Schema      []byte // JSON Schema for the LLM tool-calling menu
	Dangerous   bool   // gates on ApprovalController + strongest process caps

	// Resource-arg declarations: which args carry a path / URL / shell command,
	// so the Go executor can enforce ToolResourcePolicy on the right fields (A1.4).
	PathArgs    []string
	URLArgs     []string
	CommandArgs []string

	// Data-store regime (ADR-0039 D8 Regime 1): the scope tags this tool reads /
	// writes, when it touches the tagged stores. Empty ⇒ not a data tool.
	DataReadKinds  []string
	DataWriteKinds []string

	// ClassificationTags describe WHAT DOMAIN this tool touches — `crm`,
	// `filesystem`, `payments`, `email` (ADR-0085 D2). They are the tool's side of
	// the same tag algebra memory and skills use; they say nothing about what the
	// invocation does, which is what Effects is for.
	ClassificationTags []string

	// Effects are the closed-set verb classes this invocation exercises
	// (ADR-0086). A tool invocation is permitted only if the tag predicate passes
	// AND every declared effect is granted.
	//
	// `egress` is the one a sovereign-deployment customer cares most about: "no
	// tool may transmit outside this network" becomes a single checkable policy
	// statement rather than an audit of every tool.
	Effects []ToolEffect

	// EffectsInferred marks a tool whose effects were DERIVED from its other
	// manifest fields rather than declared (see InferEffects). It is surfaced to
	// operators so the un-migrated tools are enumerable, and it is what strict
	// mode refuses to produce.
	EffectsInferred bool
}

// ToolGrant authorizes one agent to call one tool, bounded by a resource policy
// (operator-set, A1.5). The grant is the unit ToolExecutor enforces (ADR-0039 D5).
type ToolGrant struct {
	Tool   string             `json:"tool"`
	Policy ToolResourcePolicy `json:"policy"`
}

// GrantsProvider returns the tool grants for an agent. An unknown/empty
// principal must yield no grants (fail-closed).
type GrantsProvider interface {
	GrantsFor(ctx context.Context, agentID string) ([]ToolGrant, error)
}

// ToolRegistry is the kernel-owned catalog of system tools.
type ToolRegistry interface {
	Register(t SystemTool)
	Get(name string) (SystemTool, bool)
	// SchemasFor returns the tools (name+schema) an agent may see, given its grants.
	SchemasFor(grants []ToolGrant) []SystemTool
	// All returns every registered tool. Used to build the prompt menu for an
	// agent under the unrestricted bypass (every tool is callable).
	All() []SystemTool
}

// ToolCall is a fully-authorized invocation handed to a handler. By the time a
// handler sees it, the executor has already validated the schema, the grant,
// the resource policy, the scope regime, and (if dangerous) approval.
type ToolCall struct {
	ToolName string
	ArgsJSON []byte
	Policy   ToolResourcePolicy // passed for in-handler confinement (backstop, not the gate)
}

// ToolHandler executes an already-authorized tool call. The real implementation
// invokes a confined Python tool process (A1.2); tests use a fake.
type ToolHandler interface {
	Execute(ctx context.Context, call ToolCall) (resultJSON []byte, err error)
}

// InMemoryToolRegistry is a simple registry; the BBolt-backed registry (A1.1)
// loads SystemTools from the tools bucket into one of these at startup.
type InMemoryToolRegistry struct {
	mu    sync.RWMutex
	tools map[string]SystemTool
}

// NewInMemoryToolRegistry constructs an empty registry.
func NewInMemoryToolRegistry() *InMemoryToolRegistry {
	return &InMemoryToolRegistry{tools: map[string]SystemTool{}}
}

// Register normalizes a tool's effect classes and stores it. This is the single
// registration chokepoint, so no path can put an unclassified tool in front of
// the executor: a tool arriving with no effects has them inferred (ADR-0086), and
// one declaring an effect OUTSIDE the closed set is refused outright.
//
// Refusal, not silent repair, is the right failure here: an unrecognised effect
// is a manifest a policy cannot reason about, and a downstream "unknown tool" is
// an honest answer. Strict mode (which also refuses ABSENT effects) is applied by
// the caller before it gets here — see discovery.LoadRegistry.
func (r *InMemoryToolRegistry) Register(t SystemTool) {
	normalized, err := ValidateRegistration(t, false)
	if err != nil {
		slog.Error("ADR-0086: refusing to register a tool with an invalid effect declaration",
			"tool", t.Name, "err", err)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[normalized.Name] = normalized
}

// Remove deletes a tool from the registry (ADR-0043/0044: an MCP server that
// drops or stops advertising a tool). A no-op for an unknown name.
func (r *InMemoryToolRegistry) Remove(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tools, name)
}

func (r *InMemoryToolRegistry) Get(name string) (SystemTool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

func (r *InMemoryToolRegistry) SchemasFor(grants []ToolGrant) []SystemTool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]SystemTool, 0, len(grants))
	for _, g := range grants {
		if t, ok := r.tools[g.Tool]; ok {
			out = append(out, t)
		}
	}
	return out
}

// All returns every registered tool (unordered).
func (r *InMemoryToolRegistry) All() []SystemTool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]SystemTool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	return out
}

// InMemoryGrantsStore is a simple GrantsProvider; the Postgres-backed store
// (0039-02) replaces it. Safe for concurrent use.
type InMemoryGrantsStore struct {
	mu     sync.RWMutex
	grants map[string][]ToolGrant
}

// NewInMemoryGrantsStore constructs an empty grants store.
func NewInMemoryGrantsStore() *InMemoryGrantsStore {
	return &InMemoryGrantsStore{grants: map[string][]ToolGrant{}}
}

// Set replaces an agent's grants (operator action).
func (s *InMemoryGrantsStore) Set(agentID string, grants []ToolGrant) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grants[agentID] = grants
}

// All returns a snapshot of every agent's grants — the operator plane's
// tool→agents reverse index (ADR-0047 Amendment A2.3). The map and its slices
// are copies; mutating them does not touch the store.
func (s *InMemoryGrantsStore) All() map[string][]ToolGrant {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string][]ToolGrant, len(s.grants))
	for agent, grants := range s.grants {
		cp := make([]ToolGrant, len(grants))
		copy(cp, grants)
		out[agent] = cp
	}
	return out
}

// GrantsFor returns an agent's grants. An empty agentID yields none (fail-closed).
func (s *InMemoryGrantsStore) GrantsFor(_ context.Context, agentID string) ([]ToolGrant, error) {
	if agentID == "" {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.grants[agentID], nil
}
