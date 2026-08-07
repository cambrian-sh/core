package memory

import (
	"context"

	"github.com/cambrian-sh/core/domain"
)

// llmTripletExtractor is the original LLM-backed extractor (the Tier-3 residue
// producer). It stamps every triplet sources={llm}, confidence=0 (filler) per
// the ADR-0053 D2 (revised) scale — the LLM alone is the lowest-trust tier.
type llmTripletExtractor struct {
	gen domain.Generator
}

func (e *llmTripletExtractor) ExtractBatch(ctx context.Context, texts []string, _ []string) [][]domain.ChunkTriplet {
	out := make([][]domain.ChunkTriplet, len(texts))
	if e.gen == nil || len(texts) == 0 {
		return out
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
		return out
	}
	prompt := buildBatchTripletPrompt(nonEmpty)
	resp, err := callTripletsLLM(ctx, e.gen, prompt)
	if err != nil {
		return out
	}
	perChunk := parseBatchedTripletResponse(resp, len(nonEmpty))
	for j, triplets := range perChunk {
		if j >= len(origIdx) {
			break
		}
		out[origIdx[j]] = stampSource(triplets, "llm", confFiller)
	}
	return out
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
