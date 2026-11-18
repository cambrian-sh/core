package app

import (
	"context"
	"log/slog"
	"sync"

	"github.com/cambrian-sh/core/domain"
	mcp "github.com/cambrian-sh/core/internal/infrastructure/mcp"
)

// mcpToolSink keeps the tool registry AND the ADR-0044 retrieval index in step as
// MCP servers (re)connect and drop. It tracks the tools currently published per
// server so a drop, or a changed tool list on reconnect, removes exactly the
// stale entries (registry de-registration = menu-gating; vector removal = re-sync).
type mcpToolSink struct {
	reg      *domain.InMemoryToolRegistry
	indexer  *domain.ToolIndexer
	mu       sync.Mutex
	byServer map[string]map[string]bool // serverID -> set of published tool names
	// onChange runs after a server's tool set changed (registered or dropped),
	// outside the sink's lock. It re-expands profile agents' tool grants (agent
	// stack v2), since MCP tools register after boot. nil ⇒ nothing to do.
	onChange func(ctx context.Context)
}

func (s *mcpToolSink) changed(ctx context.Context) {
	if s.onChange != nil {
		s.onChange(ctx)
	}
}

func newMCPToolSink(reg *domain.InMemoryToolRegistry, indexer *domain.ToolIndexer) *mcpToolSink {
	return &mcpToolSink{reg: reg, indexer: indexer, byServer: map[string]map[string]bool{}}
}

// Seed records the MCP tools already registered at boot, so a later drop knows
// exactly which entries belong to a server.
func (s *mcpToolSink) Seed(tools []domain.SystemTool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range tools {
		if sid, _, ok := mcp.ParseToolName(t.Name); ok {
			if s.byServer[sid] == nil {
				s.byServer[sid] = map[string]bool{}
			}
			s.byServer[sid][t.Name] = true
		}
	}
}

// SetServerTools replaces a server's published tools: registers + indexes the new
// set, and removes any previously-published tool no longer present.
func (s *mcpToolSink) SetServerTools(ctx context.Context, serverID string, tools []domain.SystemTool) {
	defer s.changed(ctx) // deferred first ⇒ runs after the unlock below
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]bool, len(tools))
	for _, t := range tools {
		next[t.Name] = true
		s.reg.Register(t)
		if err := s.indexer.Index(ctx, t); err != nil {
			slog.Warn("ADR-0044: re-index on resync failed", "tool", t.Name, "err", err)
		}
	}
	for name := range s.byServer[serverID] {
		if !next[name] {
			s.reg.Remove(name)
			_ = s.indexer.Remove(ctx, name)
		}
	}
	s.byServer[serverID] = next
}

// RemoveServerTools drops all of a server's tools from the registry and the index.
func (s *mcpToolSink) RemoveServerTools(ctx context.Context, serverID string) {
	defer s.changed(ctx) // deferred first ⇒ runs after the unlock below
	s.mu.Lock()
	defer s.mu.Unlock()
	for name := range s.byServer[serverID] {
		s.reg.Remove(name)
		_ = s.indexer.Remove(ctx, name)
	}
	delete(s.byServer, serverID)
}

var _ mcp.ToolSink = (*mcpToolSink)(nil)
