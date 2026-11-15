package mcp

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/cambrian-sh/core/domain"
)

// Persisted per_user tool lists (ADR-0140; ENG-743,
// T-20261106-per-user-mcp-servers-show-no-tools-until-the).
//
// A per_user server has no shared session: its tools are discovered through
// the first connected person's session (watchUserServer). Before that, and
// after every restart until someone with a live account exists, the server
// had NO tools on the menu, so an agent could not even reach the "connect
// your account" answer. The connector therefore keeps the last list the
// server ADVERTISED (names, descriptions, input schemas: nothing secret,
// nothing per-person) and publishes it when the server is armed.
//
// Only the advertised half is stored. The policy (dangerous flag, effects,
// classification) is re-applied from the CURRENT config on every publish
// (toolsFor), so a policy edit is never shadowed by a stale stored copy.

// WithToolLists persists per_user tool lists in store, one record per server
// id. A store error is logged and the list is simply not remembered: the
// server then behaves as before this existed (no tools until discovery).
func WithToolLists(store domain.SealedRecordStore) ConnectorOption {
	return func(c *Connector) { c.toolLists = store }
}

func (c *Connector) saveToolList(serverID string, adv []advertisedTool) {
	if c.toolLists == nil {
		return
	}
	b, err := json.Marshal(adv)
	if err == nil {
		err = c.toolLists.PutSealed(serverID, b)
	}
	if err != nil {
		slog.Warn("ADR-0140: per-user MCP tool list not persisted", "server", serverID, "err", err)
	}
}

func (c *Connector) loadToolList(serverID string) ([]advertisedTool, bool) {
	if c.toolLists == nil {
		return nil, false
	}
	b, ok, err := c.toolLists.GetSealed(serverID)
	if err != nil {
		slog.Warn("ADR-0140: per-user MCP tool list unreadable", "server", serverID, "err", err)
		return nil, false
	}
	if !ok {
		return nil, false
	}
	var adv []advertisedTool
	if err := json.Unmarshal(b, &adv); err != nil {
		slog.Warn("ADR-0140: per-user MCP tool list unreadable", "server", serverID, "err", err)
		return nil, false
	}
	return adv, true
}

// forgetToolList drops a removed server's list, so a server re-added under
// the same id later does not start from another server's tools.
func (c *Connector) forgetToolList(serverID string) {
	if c.toolLists == nil {
		return
	}
	if _, err := c.toolLists.DeleteSealed(serverID); err != nil {
		slog.Warn("ADR-0140: per-user MCP tool list not deleted", "server", serverID, "err", err)
	}
}

// publishKnownTools publishes a per_user server's remembered tools under the
// current policy and returns how many. Every call through them selects the
// caller's own session (SessionForCall), so a caller who has not connected
// gets the NotConnectedError, never another person's credential.
func (c *Connector) publishKnownTools(ctx context.Context, s ServerConfig, sink ToolSink) int {
	adv, ok := c.loadToolList(s.ID)
	if !ok || len(adv) == 0 || sink == nil {
		return 0
	}
	sink.SetServerTools(ctx, s.ID, toolsFor(s, adv))
	c.users.mu.Lock()
	c.users.published[s.ID] = true
	c.users.mu.Unlock()
	slog.Info("ADR-0140: per-user MCP server tools published from the last discovery", "server", s.ID, "tools", len(adv))
	return len(adv)
}
