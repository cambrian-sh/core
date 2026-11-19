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

	"github.com/cambrian-sh/core/domain"
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
	// ADR-0140: a per_user server's session depends on WHO the call is for
	// (the run principal on ctx), so a source that can select per call is
	// asked per call; a plain SessionSource serves the shared session.
	var (
		sess    *mcpsdk.ClientSession
		release = func(bool) {}
	)
	if cs, ok := h.Sessions.(CallSessionSource); ok {
		var err error
		sess, release, err = cs.SessionForCall(ctx, serverID)
		if err != nil {
			return nil, err
		}
	} else {
		var ok bool
		sess, ok = h.Sessions.Session(serverID)
		if !ok {
			return nil, fmt.Errorf("mcp server %q not connected", serverID)
		}
	}

	var args any
	if len(call.ArgsJSON) > 0 {
		if err := json.Unmarshal(call.ArgsJSON, &args); err != nil {
			release(false)
			return nil, fmt.Errorf("mcp tool args: %w", err)
		}
	}

	if h.CallTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, h.CallTimeout)
		defer cancel()
	}
	params := &mcpsdk.CallToolParams{Name: toolName, Arguments: args}
	if len(call.Meta) > 0 {
		// Executor-set metadata only (domain.ToolCall.Meta: the staged-inputs
		// folders); model-written arguments never reach _meta.
		params.Meta = mcpsdk.Meta(call.Meta)
	}
	res, err := sess.CallTool(ctx, params)
	// A transport failure retires a per-principal session (its process or
	// connection is suspect); a tool-level error (res.IsError) does not.
	release(err != nil && ctx.Err() == nil)
	if err != nil {
		return nil, fmt.Errorf("mcp call %q: %w", toolName, err)
	}
	// A tool-level error (res.IsError) is not a transport failure (the session
	// stays), but it IS a failed call: returned as the call's error, so the kernel
	// records it as failed (action ledger, write record, run ledger) and the model
	// still sees the tool's own message. It used to come back as an ordinary
	// result, which reported a failed write as done (ADR-0145 D14: refusals and
	// failures are never reported as success). The typed error tells the
	// ToolExecutor the message is the server's own text, so it is scanned and
	// fenced like a result (ADR-0043 D4, ENG-117).
	if res.IsError {
		return nil, &domain.ToolReportedError{Tool: toolName, Message: errorText(res)}
	}
	// The result is returned raw: the ToolExecutor scans every MCP response and
	// the agent-facing boundary fences it (domain.AgentToolResult), because kernel
	// consumers parse this JSON after dispatch (ADR-0043 D4, ENG-117).
	return marshalResult(res)
}

// errorText is the text of an error result's content (its text blocks joined),
// or the content as JSON when it has no text block.
func errorText(res *mcpsdk.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if t, ok := c.(*mcpsdk.TextContent); ok && strings.TrimSpace(t.Text) != "" {
			parts = append(parts, strings.TrimSpace(t.Text))
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "; ")
	}
	if b, err := marshalResult(res); err == nil {
		return string(b)
	}
	return "no detail"
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
