package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// llmTripletExtractor is the original LLM-backed extractor (the Tier-3 residue
// producer). It stamps every triplet sources={llm}, confidence=0 (filler) per
// the ADR-0053 D2 (revised) scale — the LLM alone is the lowest-trust tier.
//
// It speaks one of two reply formats (execution.ingestion.kg_triplets_llm_format):
//   - completion (the default): the KG²RAG base-model completion prompt
//     ("Triplets N: <h##r##t>$$..."), parsed positionally by its "Triplets N" marker;
//   - structured (ADR-0139 C1/C4): the library prompt kg_triplets as a cached system
//     block, the chunks as tagged data keyed by id, and a response schema; the reply
//     is keyed by chunk id, never read by position.
//
// Both go through the same per-triplet filters (appendChunkTriplet), so the triplets
// mean the same thing either way; whether the MODEL extracts the same triplets in the
// two formats is an empirical question that decides the default (an A/B/A retrieval
// run, AGENTS.md §0.5), which is why completion stays the default.
type llmTripletExtractor struct {
	gen        domain.Generator
	structured bool
}

func (e *llmTripletExtractor) ExtractBatch(ctx context.Context, texts []string, _ []string) ([][]domain.ChunkTriplet, error) {
	out := make([][]domain.ChunkTriplet, len(texts))
	if len(texts) == 0 {
		return out, nil
	}
	if e.gen == nil {
		// No generator wired: nothing can extract this batch. An error, not an
		// empty result, so the chunks settle as failed rather than "no triplets".
		return nil, fmt.Errorf("llm triplet extractor: no generator: %w", domain.ErrTripletExtractorUnavailable)
	}
	// Filter blanks, remember the index map (same as the prior batcher logic).
	var nonEmpty []string
	var origIdx []int
	for i, t := range texts {
		if trimmedBlank(t) {
			continue
		}
		nonEmpty = append(nonEmpty, t)
		origIdx = append(origIdx, i)
	}
	if len(nonEmpty) == 0 {
		return out, nil
	}
	var perChunk [][]domain.ChunkTriplet
	if e.structured {
		resp, err := domain.GenerateVia(ctx, e.gen, buildTripletRequest(nonEmpty))
		if err != nil {
			return nil, fmt.Errorf("llm triplet extractor: %w", err)
		}
		if perChunk, err = parseTripletReply(resp, len(nonEmpty)); err != nil {
			return nil, fmt.Errorf("llm triplet extractor: %w", err)
		}
	} else {
		prompt := buildBatchTripletPrompt(nonEmpty)
		resp, err := callTripletsLLM(ctx, e.gen, prompt)
		if err != nil {
			return nil, fmt.Errorf("llm triplet extractor: %w", err)
		}
		perChunk = parseBatchedTripletResponse(resp, len(nonEmpty))
	}
	for j, triplets := range perChunk {
		if j >= len(origIdx) {
			break
		}
		out[origIdx[j]] = stampSource(triplets, "llm", confFiller)
	}
	return out, nil
}

// fenceChunk is one chunk's text as it reaches the triplet extractor, in either
// format: the text of an ingested document (an upload, a webhook, a poller body)
// inside the ADR-0063 payload fence marked provenance="ingested_document" (ENG-94).
// The fence replaces closing-tag neutralisation: the text is one JSON-encoded,
// HTML-escaped line between nonce lines, so neither a fake </chunk> nor a forged
// fence line can end its region, and the fence itself tells the model the text is
// data.
func fenceChunk(text string) string {
	return domain.FenceUntrusted(domain.DataProvenance{Class: domain.ProvenanceIngestedDocument, Source: domain.SourceIngested}, text)
}

// buildTripletRequest renders the structured extraction call. Chunk i (0-based) is
// sent as <chunk id="i+1">, and the reply names each chunk by that id.
func buildTripletRequest(texts []string) domain.GenerateRequest {
	var sb strings.Builder
	for i, t := range texts {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(`<chunk id="`)
		sb.WriteString(strconv.Itoa(i + 1))
		sb.WriteString("\">\n")
		sb.WriteString(fenceChunk(t))
		sb.WriteString("\n</chunk>")
	}
	return kgTripletsPrompts.mustRequest(map[string]string{"chunks": sb.String()})
}

// tripletsReply is the structured reply (prompts/kg_triplets/triplets.schema.json).
type tripletsReply struct {
	Chunks []tripletsReplyChunk `json:"chunks"`
}

type tripletsReplyChunk struct {
	ID       int                `json:"id"`
	Triplets []tripletsReplyRow `json:"triplets"`
}

type tripletsReplyRow struct {
	Subject  string `json:"subject"`
	Relation string `json:"relation"`
	Object   string `json:"object"`
}

// parseTripletReply maps a structured reply back onto the n chunks BY ID: an id
// outside 1..n is ignored, a chunk the reply leaves out has no triplets, and a chunk
// listed twice has its lists merged (deduplicated). A reply that is not the expected
// JSON is an error, so its chunks settle as failed rather than as "no triplets" —
// the legacy format could not tell those apart. A reply the provider layer did not
// check against the schema (resp.SchemaUnchecked) is read tolerantly.
func parseTripletReply(resp domain.GenerateResponse, n int) ([][]domain.ChunkTriplet, error) {
	raw, err := replyField(resp.Text, "chunks", resp.SchemaUnchecked())
	if err != nil {
		return nil, err
	}
	var chunks []tripletsReplyChunk
	if err := json.Unmarshal(raw, &chunks); err != nil {
		return nil, fmt.Errorf("triplet reply is not the expected JSON: %w", err)
	}
	out := make([][]domain.ChunkTriplet, n)
	seen := make([]map[string]bool, n)
	for _, c := range chunks {
		i := c.ID - 1
		if i < 0 || i >= n {
			continue
		}
		if seen[i] == nil {
			seen[i] = map[string]bool{}
		}
		for _, t := range c.Triplets {
			// The completion format could not carry '<' or '>' inside a field (its
			// grammar is <h##r##t>), so neither can this one: same triplets either way.
			if strings.ContainsAny(t.Subject+t.Relation+t.Object, "<>") {
				continue
			}
			out[i] = appendChunkTriplet(out[i], seen[i], t.Subject, t.Relation, t.Object)
		}
	}
	return out, nil
}

// Confidence tiers (ADR-0053 D2 revised). Pointer helpers so the nullable
// SMALLINT column stays NULL for legacy rows but carries a real value here.
const (
	confFiller = 0 // produced only by the LLM residue tier
	confLow    = 1 // a single tier produced it
	confHigh   = 2 // >=2 tiers agreed, or a high-precision deterministic pattern
)

func confPtr(v int) *int { return &v }

// stampSource sets sources + confidence on every triplet that doesn't already
// carry provenance (an adapter that already labelled its output is left alone).
func stampSource(triplets []domain.ChunkTriplet, source string, confidence int) []domain.ChunkTriplet {
	for i := range triplets {
		if len(triplets[i].Sources) == 0 {
			triplets[i].Sources = []string{source}
		}
		if triplets[i].Confidence == nil {
			triplets[i].Confidence = confPtr(confidence)
		}
	}
	return triplets
}

func trimmedBlank(s string) bool {
	for _, r := range s {
		if r != ' ' && r != '\t' && r != '\n' && r != '\r' {
			return false
		}
	}
	return true
}
