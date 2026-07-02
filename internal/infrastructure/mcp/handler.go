// Package mcp is the infrastructure adapter that lets Cambrian consume tools from
// external MCP (Model Context Protocol) servers behind the ADR-0039 ToolExecutor
// (ADR-0043). The official MCP Go SDK (github.com/modelcontextprotocol/go-sdk) is
// imported ONLY here, keeping domain protocol-agnostic (separability rule).
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// ToolNamePrefix marks a System Tool sourced from an MCP server. An MCP tool's
// identity is mcp:<server-id>/<tool-name> (ADR-0043 D3).
const ToolNamePrefix = "mcp:"

// SessionSource resolves a connected MCP client session for a server id. The
// connection/health manager (slice 0043-05) implements this; the Handler stays
// transport-agnostic and unaware of how sessions are established or kept warm.
type SessionSource interface {
	Session(serverID string) (*mcpsdk.ClientSession, bool)
}

// Handler invokes external MCP tools (ADR-0043 D1). It implements
// domain.ToolHandler — the same interface ProcessHandler implements — so MCP
// tools are authorized, scoped, approved, and metered by the one ToolExecutor
// exactly like native tools. A missing session or a call failure is returned as
// an error (the ToolExecutor turns it into structured response data); it never
// panics into the reasoning loop.
type Handler struct {
	Sessions SessionSource
	// CallTimeout bounds each MCP tool call (ADR-0043 D8). 0 ⇒ no per-call
	// deadline; the kernel is the timeout authority, like ProcessHandler.
	CallTimeout time.Duration
}

// Execute calls the MCP tool named by call.ToolName (mcp:<server>/<tool>) on the
// resolved server session and returns the result as JSON.
func (h *Handler) Execute(ctx context.Context, call domain.ToolCall) ([]byte, error) {
	serverID, toolName, ok := ParseToolName(call.ToolName)
	if !ok {
		return nil, fmt.Errorf("not an MCP tool name: %q", call.ToolName)
	}
	if h.Sessions == nil {
		return nil, fmt.Errorf("mcp: no session source configured")
	}
	sess, ok := h.Sessions.Session(serverID)
	if !ok {
		return nil, fmt.Errorf("mcp server %q not connected", serverID)
	}

	var args any
	if len(call.ArgsJSON) > 0 {
		if err := json.Unmarshal(call.ArgsJSON, &args); err != nil {
			return nil, fmt.Errorf("mcp tool args: %w", err)
		}
	}

	if h.CallTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, h.CallTimeout)
		defer cancel()
	}
	res, err := sess.CallTool(ctx, &mcpsdk.CallToolParams{Name: toolName, Arguments: args})
	if err != nil {
		return nil, fmt.Errorf("mcp call %q: %w", toolName, err)
	}
	// A tool-level error (res.IsError) is data, not a transport failure — the MCP
	// spec carries it in Content so the model can see it; we return it as the
	// result bytes and let the reasoning loop reconcile.
	return marshalResult(res)
}

// ParseToolName splits mcp:<server>/<tool> into its parts. ok=false for any name
// that is not a well-formed MCP tool identity.
func ParseToolName(name string) (server, tool string, ok bool) {
	rest, found := strings.CutPrefix(name, ToolNamePrefix)
	if !found {
		return "", "", false
	}
	server, tool, found = strings.Cut(rest, "/")
	if !found || server == "" || tool == "" {
		return "", "", false
	}
	return server, tool, true
}

// marshalResult renders a CallToolResult to JSON, preferring the structured
// output when present, else the unstructured content blocks.
func marshalResult(res *mcpsdk.CallToolResult) ([]byte, error) {
	if res.StructuredContent != nil {
		return json.Marshal(res.StructuredContent)
	}
	return json.Marshal(res.Content)
}
