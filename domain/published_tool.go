package domain

import (
	"context"
	"encoding/json"
	"regexp"
)

// ─────────────────────────────────────────────────────────────────────────────
// The Published Tool Surface (ADR-0126 D1) — the OUTBOUND tool port.
//
// This file is protocol-agnostic on purpose. MCP is one RENDERER of the surface,
// not its definition: the SDK stays in internal/, and a second renderer (an
// HTTP/OpenAPI surface, an ACP adapter) is additive rather than a rewrite.
//
// It is deliberately NOT SystemTool + InMemoryToolRegistry, which face the other
// way. Fusing them has two concrete failure modes: every published tool would
// appear in INTERNAL agents' tool menus (an agent-callable action with no tool
// grant behind it), and every foreign MCP tool already in that registry
// (`mcp:<server>/<tool>`) would be re-published outward, making Cambrian an open
// proxy for whatever the operator dialled. The effect vocabulary IS shared
// (ToolEffect) so policy stays one thing; the registries are not.
// ─────────────────────────────────────────────────────────────────────────────

// PublishedTool is a tool Cambrian OFFERS to external callers (ADR-0126).
// It is the outbound counterpart of SystemTool, which is a tool Cambrian's own
// agents may CALL. The two never mix — see ADR-0126 D2.
type PublishedTool struct {
	// Name is the caller-visible tool id, matching PublishedToolNamePattern.
	// snake_case and never dotted (D7): a dotted name is a bet on every
	// downstream client's tokenizer and name-validator, and this codebase has
	// already lost that bet once (ADR-0097 D7).
	Name string
	// Title is the human label for a tool picker.
	Title string
	// Description is written for an LLM caller, not an operator — it is the only
	// thing standing between a published tool and never being called.
	Description string
	// InputSchema is JSON Schema, object-typed at the top level.
	InputSchema []byte
	// Effects are the ADR-0086 CLOSED verb classes this invocation exercises,
	// reused verbatim from the internal tool plane so policy stays one thing.
	// They are what a per-caller `tools/list` filter narrows on (D8).
	Effects []ToolEffect
	// ReadOnly maps to the MCP readOnlyHint annotation.
	ReadOnly bool
	// Capability is the operator capability this tool rides on ("" = always).
	Capability string
	// MachineOnly restricts the tool to worker machines (ADR-0127 D3:
	// poll_step/report_step are callable ONLY by machine:* principals). The
	// transport enforces it twice — the listing filter hides the tool from
	// everyone else, the call side refuses them — and the handler checks a
	// third time, because a handler that trusted the menu would be trusting
	// a filter it cannot see.
	MachineOnly bool
}

// PublishedToolHandler serves one published tool.
type PublishedToolHandler interface {
	// Invoke runs the tool for the principal already established on ctx.
	// It must never read a principal from its arguments (ADR-0126 D4) — identity
	// is bound by the transport middleware before any handler runs, and a handler
	// that trusted its arguments would be a self-asserted privilege level.
	Invoke(ctx context.Context, args json.RawMessage) (PublishedToolResult, error)
}

// PublishedToolResult is one invocation's answer, in the two shapes a caller may
// want it plus the handle that makes it auditable.
type PublishedToolResult struct {
	// Structured is marshalled to MCP structuredContent.
	Structured any
	// Text is the human/LLM-readable rendering.
	Text string
	// ReceiptRef is the ADR-0126 D6 correlation handle — "" when this deployment
	// has no receipt lane, which is the OSS default and not a failure.
	ReceiptRef string
}

// PublishedToolNamePattern is the name grammar every published tool must match
// (ADR-0126 D7). Lower-case snake_case, leading letter, at most 48 characters.
const PublishedToolNamePattern = `^[a-z][a-z0-9_]{0,47}$`

var publishedToolName = regexp.MustCompile(PublishedToolNamePattern)

// ValidPublishedToolName reports whether name matches PublishedToolNamePattern.
// Used at the registration chokepoint, so a name no client can address is a boot
// error rather than a 400 discovered by whoever dialled the endpoint first.
func ValidPublishedToolName(name string) bool {
	return publishedToolName.MatchString(name)
}

// PublishedToolEntry is one composed entry of the surface: what a tool declares,
// what serves it, and which plugin owns it. Owner is carried for attribution —
// a duplicate name has to be able to name both claimants.
type PublishedToolEntry struct {
	Owner   string
	Tool    PublishedTool
	Handler PublishedToolHandler
}

// PublishedToolSurface is the composed, order-stable read snapshot a renderer
// consumes. It lives here rather than beside the registry so that a renderer in
// internal/ can name it without importing the composition root.
type PublishedToolSurface []PublishedToolEntry
