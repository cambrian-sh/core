package network

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// RerankerDispatcher implements memory.Reranker by invoking the privileged
// `reranker_agent` system organ directly via the Auctioneer (no auction), exactly
// like the kg_extractor (ADR-0053 D2 revised) and the pre-plan Scout (ADR-0051).
//
// The kernel hands the agent a {query, documents[]} Handoff and gets one bge
// cross-encoder relevance score per document back (ADR-0054 Stage B). The model
// load + scoring live in the warm agent (agents/system/reranker_agent/); this Go side
// is just dispatch + parse, mirroring KgExtractorDispatcher.
type RerankerDispatcher struct {
	Auctioneer domain.Auctioneer
	AgentID    string // default "reranker_agent"
}

// rerankRequest is the handoff payload the agent receives.
type rerankRequest struct {
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
}

// rerankResponse is the agent's reply: one score per document, positional.
type rerankResponse struct {
	Scores []float64 `json:"scores"`
}

// Rerank sends the (query, documents) to the reranker system agent and returns the
// per-document cross-encoder scores. Unlike the kg_extractor (which fail-softs to
// empty), this returns an ERROR on any dispatch/parse failure: the caller
// (applyStageBRerank) reads the error as "keep the Stage-A order", so the failure
// signal must propagate rather than masquerade as an all-zero rerank (which would
// silently flatten the ranking).
func (d *RerankerDispatcher) Rerank(ctx context.Context, query string, documents []string) ([]float64, error) {
	if d == nil || d.Auctioneer == nil {
		return nil, fmt.Errorf("reranker: no auctioneer wired")
	}
	if len(documents) == 0 {
		return nil, nil
	}
	agentID := d.AgentID
	if agentID == "" {
		agentID = "reranker_agent"
	}
	reqData, err := json.Marshal(rerankRequest{Query: query, Documents: documents})
	if err != nil {
		return nil, fmt.Errorf("reranker: marshal request: %w", err)
	}
	h := &domain.Handoff{
		FromAgent: "orchestrator",
		ToAgent:   agentID,
		Payload:   &domain.Payload{Type: "rerank_request", Data: reqData},
		Context:   map[string]string{"task_id": "rerank"},
	}
	resp, err := d.Auctioneer.CallAgent(ctx, agentID, h, "")
	if err != nil {
		return nil, fmt.Errorf("reranker: dispatch: %w", err)
	}
	if resp == nil || resp.Payload == nil || len(resp.Payload.Data) == 0 {
		return nil, fmt.Errorf("reranker: empty response")
	}
	var parsed rerankResponse
	if err := json.Unmarshal(resp.Payload.Data, &parsed); err != nil {
		slog.Warn("reranker dispatch: bad response JSON", "err", err)
		return nil, fmt.Errorf("reranker: unmarshal response: %w", err)
	}
	return parsed.Scores, nil
}
