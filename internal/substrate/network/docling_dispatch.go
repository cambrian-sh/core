package network

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/memory"
)

// DoclingDispatcher implements memory.StructureParser by invoking the privileged,
// deterministic docling_agent directly via the Auctioneer (no auction), exactly
// like the kg_extractor dispatcher (ADR-0060). The agent parses a document into
// its real hierarchy; this Go side is just dispatch + JSON parse.
type DoclingDispatcher struct {
	Auctioneer domain.Auctioneer
	AgentID    string // default "docling_agent"
}

type doclingRequest struct {
	DocID      string `json:"doc_id"`
	Title      string `json:"title,omitempty"`
	SourceType string `json:"source_type,omitempty"`
	Text       string `json:"text,omitempty"`
	DataB64    string `json:"data_b64,omitempty"`
}

// Parse sends the document to the docling_agent and returns the structured tree.
// Never panics: a dispatch/parse failure returns (nil, err) and the caller falls
// back to flat chunking (the chunk docs are already saved — only the structure
// graph is lost).
func (d *DoclingDispatcher) Parse(ctx context.Context, req memory.StructureParseRequest) (*memory.StructuredDocument, error) {
	if d == nil || d.Auctioneer == nil {
		return nil, nil
	}
	agentID := d.AgentID
	if agentID == "" {
		agentID = "docling_agent"
	}
	reqData, err := json.Marshal(doclingRequest{
		DocID: req.DocID, Title: req.Title, SourceType: req.SourceType,
		Text: req.Text, DataB64: req.DataB64,
	})
	if err != nil {
		return nil, err
	}
	h := &domain.Handoff{
		FromAgent: "orchestrator",
		ToAgent:   agentID,
		Payload:   &domain.Payload{Type: "structure_request", Data: reqData},
		Context:   map[string]string{"task_id": "docling-parse"},
	}
	resp, err := d.Auctioneer.CallAgent(ctx, agentID, h, "")
	if err != nil || resp == nil || resp.Payload == nil || len(resp.Payload.Data) == 0 {
		slog.DebugContext(ctx, "docling dispatch: no structure returned", "err", err)
		return nil, err
	}
	var sd memory.StructuredDocument
	if err := json.Unmarshal(resp.Payload.Data, &sd); err != nil {
		slog.WarnContext(ctx, "docling dispatch: bad response JSON", "err", err)
		return nil, err
	}
	return &sd, nil
}
