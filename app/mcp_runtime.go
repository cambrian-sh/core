package app

import (
	"context"
	"sync"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/mcp"
	"github.com/cambrian-sh/core/internal/sandbox"
)

// mcpServerFromConfig maps one configured MCP server to the connector's shape
// (ADR-0043 D2/D9). Extracted from the boot loop so the contract-0097 runtime
// save path builds EXACTLY the ServerConfig a restart would — two mappings
// would drift, and the drift would only show as a server that behaves
// differently live than after the next boot.
//
// sb is the boot-resolved trust-tier policy (ADR-0139 D8, mcp_sandbox.go): a
// required argument, so no mapping can build a server without containment.
func mcpServerFromConfig(s config.MCPServerConfig, sb *sandbox.Policy) mcp.ServerConfig {
	toolPolicy := make(map[string]mcp.ToolPolicy, len(s.Tools))
	for _, tc := range s.Tools {
		toolPolicy[tc.Name] = mcpToolPolicyFromConfig(tc)
	}
	// ADR-0139 D8: nil default ⇒ the connector fails closed on unlisted tools.
	var def *mcp.ToolPolicy
	if s.DefaultToolPolicy != nil {
		p := mcpToolPolicyFromConfig(*s.DefaultToolPolicy)
		def = &p
	}
	out := mcp.ServerConfig{
		ID: s.ID, Transport: s.Transport, Endpoint: s.Endpoint, Args: s.Args,
		AuthType: s.Auth.Type, AuthHeader: s.Auth.Header, AuthTokenEnv: s.Auth.TokenEnv,
		Env:                       s.Env,
		EnvValues:                 s.EnvValues,
		SecretEnv:                 s.SecretEnv,
		Tools:                     toolPolicy,
		DefaultPolicy:             def,
		DefaultClassificationTags: s.ClassificationTags,
		Sandbox:                   containmentSpec(s, sb),
		AllowPrivateNetwork:       s.AllowPrivateNetwork,
	}
	// ADR-0140: the credential model travels with the server, so the
	// connector selects a per-principal session for per_user / either.
	if uc := s.UserCredential; uc != nil {
		out.CredentialScope = uc.Scope
		out.UserInject, out.UserInjectName = uc.Inject, uc.Name
		out.SharedFallback = uc.SharedFallback
	}
	return out
}

// mcpToolPolicyFromConfig maps one configured tool policy (per-tool or the
// server default) to the connector's shape.
func mcpToolPolicyFromConfig(tc config.MCPToolConfig) mcp.ToolPolicy {
	return mcp.ToolPolicy{
		Dangerous:          tc.Dangerous,
		DataWriteKinds:     tc.DataWriteKinds,
		ClassificationTags: tc.ClassificationTags,
		Effects:            tc.Effects,
		StagedInputs:       tc.StagedInputs,
	}
}

// mcpRuntime is the live MCP server list (contract 0097): boot config plus
// every runtime save, minus every runtime removal. The operator plane's list
// read and the write path's hot-apply share it, so a console re-read after a
// save shows the server it just added rather than the boot state it changed.
type mcpRuntime struct {
	mu      sync.RWMutex
	servers []mcp.ServerConfig
	// cfgs / sources carry the integrations view of each server: the declared
	// config entry (enabled flag, settings, secret names, catalog provenance)
	// and where it came from ("store" | "file" | "plugin"). A plugin server has
	// no config entry; its cfgs slot is absent.
	cfgs    map[string]config.MCPServerConfig
	sources map[string]string
	// sandbox is the boot-resolved trust-tier policy every server built from
	// config is launched under (ADR-0139 D8). Set once at construction.
	sandbox *sandbox.Policy
}

// containment is a server's effective containment for status surfaces: the
// last launch's report, else what it would get (planned, not started).
func (r *mcpRuntime) containment(ctx context.Context, conn *mcp.Connector, id string) (sandbox.Report, bool) {
	if conn != nil {
		if rep, ok := conn.Containment(id); ok {
			return rep, true
		}
	}
	s, ok := r.get(id)
	if !ok || s.Transport != "stdio" {
		return sandbox.Report{}, false
	}
	return sandbox.Plan(ctx, s.Sandbox, sandbox.Command{Path: s.Endpoint}), true
}

// mcpSource values for mcpRuntime.sources.
const (
	mcpSourceStore  = "store"
	mcpSourceFile   = "file"
	mcpSourcePlugin = "plugin"
)

// upsertConfig records a configured (non-plugin) server with its source.
func (r *mcpRuntime) upsertConfig(c config.MCPServerConfig, source string) {
	r.upsert(mcpServerFromConfig(c, r.sandbox))
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cfgs == nil {
		r.cfgs = map[string]config.MCPServerConfig{}
	}
	if r.sources == nil {
		r.sources = map[string]string{}
	}
	r.cfgs[c.ID] = c
	r.sources[c.ID] = source
}

// setSource records a server's source (plugin servers, boot).
func (r *mcpRuntime) setSource(id, source string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sources == nil {
		r.sources = map[string]string{}
	}
	r.sources[id] = source
}

// config returns a server's declared config entry and its source.
func (r *mcpRuntime) config(id string) (config.MCPServerConfig, string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.cfgs[id]
	return c, r.sources[id], ok
}

// enabled reports whether a server should run: plugin servers always do.
func (r *mcpRuntime) enabled(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if c, ok := r.cfgs[id]; ok {
		return c.IsEnabled()
	}
	return true
}

// source reports where a server's definition comes from.
func (r *mcpRuntime) source(id string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sources[id]
}

func (r *mcpRuntime) list() []mcp.ServerConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]mcp.ServerConfig(nil), r.servers...)
}

func (r *mcpRuntime) get(id string) (mcp.ServerConfig, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.servers {
		if s.ID == id {
			return s, true
		}
	}
	return mcp.ServerConfig{}, false
}

func (r *mcpRuntime) upsert(s mcp.ServerConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.servers {
		if r.servers[i].ID == s.ID {
			r.servers[i] = s
			return
		}
	}
	r.servers = append(r.servers, s)
}

func (r *mcpRuntime) remove(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.servers {
		if r.servers[i].ID == id {
			r.servers = append(r.servers[:i], r.servers[i+1:]...)
			delete(r.cfgs, id)
			delete(r.sources, id)
			return true
		}
	}
	return false
}
