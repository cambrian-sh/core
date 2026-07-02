package domain

import (
	"context"
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

func (r *InMemoryToolRegistry) Register(t SystemTool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.Name] = t
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

// GrantsFor returns an agent's grants. An empty agentID yields none (fail-closed).
func (s *InMemoryGrantsStore) GrantsFor(_ context.Context, agentID string) ([]ToolGrant, error) {
	if agentID == "" {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.grants[agentID], nil
}
