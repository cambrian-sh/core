package app

import (
	"sync"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/mcp"
)

// mcpServerFromConfig maps one configured MCP server to the connector's shape
// (ADR-0043 D2/D9). Extracted from the boot loop so the contract-0097 runtime
// save path builds EXACTLY the ServerConfig a restart would — two mappings
// would drift, and the drift would only show as a server that behaves
// differently live than after the next boot.
func mcpServerFromConfig(s config.MCPServerConfig) mcp.ServerConfig {
	toolPolicy := make(map[string]mcp.ToolPolicy, len(s.Tools))
	for _, tc := range s.Tools {
		toolPolicy[tc.Name] = mcp.ToolPolicy{
			Dangerous:          tc.Dangerous,
			DataWriteKinds:     tc.DataWriteKinds,
			ClassificationTags: tc.ClassificationTags,
		}
	}
	return mcp.ServerConfig{
		ID: s.ID, Transport: s.Transport, Endpoint: s.Endpoint, Args: s.Args,
		AuthType: s.Auth.Type, AuthHeader: s.Auth.Header, AuthTokenEnv: s.Auth.TokenEnv,
		Tools:                     toolPolicy,
		DefaultClassificationTags: s.ClassificationTags,
	}
}

// mcpRuntime is the live MCP server list (contract 0097): boot config plus
// every runtime save, minus every runtime removal. The operator plane's list
// read and the write path's hot-apply share it, so a console re-read after a
// save shows the server it just added rather than the boot state it changed.
type mcpRuntime struct {
	mu      sync.RWMutex
	servers []mcp.ServerConfig
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
			return true
		}
	}
	return false
}
