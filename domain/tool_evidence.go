package domain

import (
	"strings"
	"time"
)

// Tool evidence provenance (ENG-715, owner decision 2026-11-15).
//
// When the kernel fetches evidence as an MCP CLIENT from an external server
// (`mcp:<server>/<tool>`), the result is written to the run ledger (ADR-0145 D1)
// and this event is published on the EventBus. It carries a content DIGEST and a
// POINTER to the ledger row that holds the result — never the result itself — so
// a downstream provenance lane (premium receipts) can prove which external
// evidence an answer used without becoming a second copy of it, and an erasure
// of the ledger row is never blocked by the record.
//
// Like DecisionObserver this is one-way: publishing cannot fail or delay the
// tool call, and with no subscriber nothing happens.

// EventTypeToolEvidenceRecorded is ToolEvidenceRecordedEvent's bus type.
const EventTypeToolEvidenceRecorded = "tool.evidence.recorded"

// ToolEvidenceRecordedEvent reports one external-MCP tool result recorded in the
// run ledger.
type ToolEvidenceRecordedEvent struct {
	// LedgerID is the pointer: the run-ledger entry holding the result.
	LedgerID string
	// ToolName is the full tool name (`mcp:<server>/<tool>`); ServerID the server.
	ToolName string
	ServerID string
	// ArgsDigest and ContentDigest are hex SHA-256 of the canonical arguments and
	// of the result bytes as recorded.
	ArgsDigest    string
	ContentDigest string
	// Provenance of the call (ADR-0145 D1 / ADR-0142 act-for).
	ConversationID string
	TurnID         string
	RunID          string
	StepIndex      int
	// Writer is the agent whose call produced the result; Owner the person the
	// work was for ("" when none).
	Writer string
	Owner  string
	At     time.Time
}

func (ToolEvidenceRecordedEvent) domainEvent()      {}
func (ToolEvidenceRecordedEvent) EventType() string { return EventTypeToolEvidenceRecorded }

// ExternalMCPServer reports the server id of an external MCP client tool name
// (`mcp:<server>/<tool>`); ok is false for any other tool.
func ExternalMCPServer(toolName string) (server string, ok bool) {
	rest, found := strings.CutPrefix(toolName, "mcp:")
	if !found {
		return "", false
	}
	server, _, found = strings.Cut(rest, "/")
	if !found || server == "" {
		return "", false
	}
	return server, true
}
