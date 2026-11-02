package network

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// KgExtractorDispatcher implements domain.TripletExtractor by invoking the
// privileged, NO-LLM kg_extractor system agent directly via the agent transport
// (no auction) — the ADR-0051 system-organ class (ADR-0053 D2 revised).
//
// The kernel hands the agent a batch of chunk texts as a Handoff and gets the
// per-chunk (h, r, t) triplets back as a Handoff. The tiered extraction loop
// (metadata + spacy_patterns) lives in the agent (agents/system/kg_extractor_agent/);
// this Go side is dispatch + parse, plus a bounded retry of the CALL.
//
// Retry (T-20261102-kg-extractor-cold-start-drops-the-first-kg): the agent is
// booted lazily by the first call, and a cold Python + spaCy start can miss the
// agent boot timeout; the first two batches after a fresh boot were lost that
// way. A failed CallAgent (boot, dial or RPC) is therefore retried up to
// Attempts times in total, sleeping Backoff, 2*Backoff, ... between attempts
// under ctx. An unusable reply (empty payload, bad JSON) is not retried: the
// agent answered, and asking again would get the same answer.
type KgExtractorDispatcher struct {
	Caller  domain.AgentCaller
	AgentID string // default "kg_extractor_agent"
	// Attempts is the total number of CallAgent attempts per batch
	// (execution.ingestion.kg_extractor_call_attempts); <=0 means 1, no retry.
	Attempts int
	// Backoff is the sleep before the second attempt, doubled for each further
	// one (execution.ingestion.kg_extractor_retry_backoff_ms); <=0 means no sleep.
	Backoff time.Duration

	// sleep is the backoff wait; nil means a ctx-aware timer. A test seam only.
	sleep func(ctx context.Context, d time.Duration) error
}

// kgRequest is the handoff payload the agent receives: positional batches of
// texts + their document ids (ids anchor the deterministic metadata triplets).
type kgRequest struct {
	Texts []string `json:"texts"`
	IDs   []string `json:"ids,omitempty"`
}

// kgTriplet mirrors domain.ChunkTriplet on the wire (the agent emits this JSON).
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

func (d *KgExtractorDispatcher) agentID() string {
	if d.AgentID == "" {
		return "kg_extractor_agent"
	}
	return d.AgentID
}

// ExtractBatch sends the texts to the kg_extractor system agent and returns the
// per-chunk triplets. It returns an error when the batch was not extracted:
// the agent could not be reached after every attempt (wrapped with
// domain.ErrTripletExtractorUnavailable), or its reply was unusable. A
// successful reply with no triplets for a text is an empty slice and no error.
func (d *KgExtractorDispatcher) ExtractBatch(ctx context.Context, texts []string, ids []string) ([][]domain.ChunkTriplet, error) {
	out := make([][]domain.ChunkTriplet, len(texts))
	if len(texts) == 0 {
		return out, nil
	}
	parsed, err := d.call(ctx, kgRequest{Texts: texts, IDs: ids})
	if err != nil {
		return nil, err
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
			out[i] = append(out[i], domain.ChunkTriplet{
				H: t.H, R: t.R, T: t.T, Weight: w,
				Sources: t.Sources, Confidence: t.Confidence,
			})
		}
	}
	return out, nil
}

// Ready implements domain.ReadinessProber: one empty-batch round trip (the
// agent answers {"triplets": []} without loading anything per call), with the
// same bounded retry as ExtractBatch. A nil error means the agent is booted,
// dialled and serving; the batcher calls it once at start so the agent's cold
// start happens at kernel boot, not inside the first ingest's batch.
func (d *KgExtractorDispatcher) Ready(ctx context.Context) error {
	_, err := d.call(ctx, kgRequest{Texts: []string{}})
	return err
}

// call runs one request with the bounded retry and parses the reply.
func (d *KgExtractorDispatcher) call(ctx context.Context, req kgRequest) (kgResponse, error) {
	if d == nil || d.Caller == nil {
		return kgResponse{}, fmt.Errorf("kg_extractor dispatch: no agent caller wired: %w", domain.ErrTripletExtractorUnavailable)
	}
	agentID := d.agentID()
	reqData, err := json.Marshal(req)
	if err != nil {
		return kgResponse{}, fmt.Errorf("kg_extractor dispatch: encode request: %w", err)
	}
	attempts := d.Attempts
	if attempts <= 0 {
		attempts = 1
	}
	var resp *domain.Handoff
	var callErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			wait := d.Backoff << (attempt - 2) // Backoff, 2*Backoff, 4*Backoff, ...
			slog.InfoContext(ctx, "kg_extractor dispatch: agent call failed, retrying",
				"agent_id", agentID, "attempt", attempt, "of", attempts, "backoff_ms", wait.Milliseconds(), "cause", callErr)
			if err := d.wait(ctx, wait); err != nil {
				return kgResponse{}, fmt.Errorf("kg_extractor dispatch: %s: gave up after %d of %d attempts (%w), last error: %v: %w",
					agentID, attempt-1, attempts, err, callErr, domain.ErrTripletExtractorUnavailable)
			}
		}
		h := &domain.Handoff{
			FromAgent: "orchestrator",
			ToAgent:   agentID,
			Payload:   &domain.Payload{Type: "chunk_triplet_request", Data: reqData},
			Context:   map[string]string{"task_id": "kg-extract"},
		}
		resp, callErr = d.Caller.CallAgent(ctx, agentID, h, "")
		if callErr == nil {
			break
		}
		if ctx.Err() != nil {
			// The batch deadline (or shutdown) ended it; another attempt cannot run.
			return kgResponse{}, fmt.Errorf("kg_extractor dispatch: %s: attempt %d of %d: %w: %w",
				agentID, attempt, attempts, callErr, domain.ErrTripletExtractorUnavailable)
		}
	}
	if callErr != nil {
		return kgResponse{}, fmt.Errorf("kg_extractor dispatch: %s unreachable after %d attempts: %w: %w",
			agentID, attempts, callErr, domain.ErrTripletExtractorUnavailable)
	}
	if resp == nil || resp.Payload == nil || len(resp.Payload.Data) == 0 {
		// The agent always answers {"triplets": [...]}, even for an empty batch,
		// so an empty reply is a broken agent, not "no triplets".
		return kgResponse{}, fmt.Errorf("kg_extractor dispatch: %s returned an empty reply", agentID)
	}
	var parsed kgResponse
	if err := json.Unmarshal(resp.Payload.Data, &parsed); err != nil {
		return kgResponse{}, fmt.Errorf("kg_extractor dispatch: %s: bad response JSON: %w", agentID, err)
	}
	return parsed, nil
}

func (d *KgExtractorDispatcher) wait(ctx context.Context, dur time.Duration) error {
	if d.sleep != nil {
		return d.sleep(ctx, dur)
	}
	if dur <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(dur)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
