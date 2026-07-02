package network

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/memory"
)

// KgExtractorDispatcher implements memory.TripletExtractor by invoking the
// privileged, NO-LLM kg_extractor system agent directly via the Auctioneer
// (no auction), exactly like the pre-plan Scout (ADR-0051 / ADR-0053 D2 revised).
//
// The kernel hands the agent a batch of chunk texts as a Handoff and gets the
// per-chunk (h, r, t) triplets back as a Handoff. The tiered extraction loop
// (metadata + spacy_patterns) lives in the agent (agents/system/kg_extractor_agent/);
// this Go side is just dispatch + parse, mirroring AgentScoutDispatcher.
type KgExtractorDispatcher struct {
	Auctioneer domain.Auctioneer
	AgentID    string // default "kg_extractor_agent"
}

// kgRequest is the handoff payload the agent receives: positional batches of
// texts + their document ids (ids anchor the deterministic metadata triplets).
type kgRequest struct {
	Texts []string `json:"texts"`
	IDs   []string `json:"ids,omitempty"`
}

// kgTriplet mirrors memory.ChunkTriplet on the wire (the agent emits this JSON).
type kgTriplet struct {
	H          string   `json:"h"`
	R          string   `json:"r"`
	T          string   `json:"t"`
	Weight     float64  `json:"weight"`
	Sources    []string `json:"sources,omitempty"`
	Confidence *int     `json:"confidence,omitempty"`
}

// kgResponse is the agent's reply: one triplet list per input text, positional.
type kgResponse struct {
	Triplets [][]kgTriplet `json:"triplets"`
}

// ExtractBatch sends the texts to the kg_extractor system agent and returns the
// per-chunk triplets. Never errors: a dispatch/parse failure yields empty
// extractions (the chunk docs are already saved upstream — only enrichment is lost),
// the same degradation contract as the LLM extractor it replaces.
func (d *KgExtractorDispatcher) ExtractBatch(ctx context.Context, texts []string, ids []string) [][]memory.ChunkTriplet {
	out := make([][]memory.ChunkTriplet, len(texts))
	if d == nil || d.Auctioneer == nil || len(texts) == 0 {
		return out
	}
	agentID := d.AgentID
	if agentID == "" {
		agentID = "kg_extractor_agent"
	}
	reqData, err := json.Marshal(kgRequest{Texts: texts, IDs: ids})
	if err != nil {
		return out
	}
	h := &domain.Handoff{
		FromAgent: "orchestrator",
		ToAgent:   agentID,
		Payload:   &domain.Payload{Type: "chunk_triplet_request", Data: reqData},
		Context:   map[string]string{"task_id": "kg-extract"},
	}
	resp, err := d.Auctioneer.CallAgent(ctx, agentID, h, "")
	if err != nil || resp == nil || resp.Payload == nil || len(resp.Payload.Data) == 0 {
		slog.Debug("kg_extractor dispatch: no triplets returned", "err", err)
		return out
	}
	var parsed kgResponse
	if err := json.Unmarshal(resp.Payload.Data, &parsed); err != nil {
		slog.Warn("kg_extractor dispatch: bad response JSON", "err", err)
		return out
	}
	for i, chunkTriplets := range parsed.Triplets {
		if i >= len(out) {
			break
		}
		for _, t := range chunkTriplets {
			if t.H == "" || t.R == "" || t.T == "" {
				continue
			}
			w := t.Weight
			if w == 0 {
				w = 1.0
			}
			out[i] = append(out[i], memory.ChunkTriplet{
				H: t.H, R: t.R, T: t.T, Weight: w,
				Sources: t.Sources, Confidence: t.Confidence,
			})
		}
	}
	return out
}
