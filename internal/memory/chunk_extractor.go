package memory

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// ChunkTriplet is one (h, r, t) triple extracted from a chunk's text by the LLM.
// This is the per-chunk KG that the KG²RAG retrieval pattern walks. The h and t
// are free-form entity strings (canonicalized to lowercase on insert); the r is
// a free-form verb phrase.
type ChunkTriplet struct {
	H      string  // head entity (canonicalized: lowercase, trimmed)
	R      string  // relation (free-form verb phrase; e.g., "researched", "born in")
	T      string  // tail entity (canonicalized)
	Weight float64 // extractor's per-triple weight, [0, 1]; 1.0 if not reported
	// ADR-0053 D2 (revised): provenance + agreement tier from the tiered extractor.
	// Sources: producers, subset of {metadata, spacy_patterns, llm}; nil = legacy.
	// Confidence: 2=high / 1=low / 0=filler; nil = unset/legacy (persisted NULL).
	Sources    []string `json:"sources,omitempty"`
	Confidence *int     `json:"confidence,omitempty"`
}

// chunkTripletRE matches the LLM's triplet output format: <h##r##t>.
// The LLM is prompted to emit triplets separated by `$$`.
var chunkTripletRE = regexp.MustCompile(`<([^<>]+)##([^<>]+)##([^<>]+)>`)

// ExtractChunkTriplets prompts the LLM to extract (h, r, t) triples from a single
// chunk's text. The output format matches the KG²RAG reference implementation
// (https://github.com/nju-websoft/KG2RAG/blob/main/code/preprocess/hotpot_extraction.py):
// the LLM emits `<h##r##t>$$<h##r##t>$$...` and we parse the brackets.
//
// The prompt is deliberately small and example-driven. The LLM is told:
//   - Only emit triplets directly from the text (no inference)
//   - Use the exact `<h##r##t>` format separated by `$$`
//   - Skip nulls, "no" / "unknown" / "null" / "NULL" placeholders
//   - Skip self-loops (h == t)
//
// Returns the parsed list. Empty list is fine (no relations to extract). The
// caller is responsible for filtering by confidence / cost-gate.
func ExtractChunkTriplets(ctx context.Context, gen domain.Generator, chunkText string) ([]ChunkTriplet, error) {
	if gen == nil || strings.TrimSpace(chunkText) == "" {
		return nil, nil
	}
	prompt := buildChunkTripletPrompt(chunkText)
	resp, err := gen.Generate(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("chunk triplet extractor: LLM call failed: %w", err)
	}
	return parseChunkTripletOutput(resp), nil
}

// buildChunkTripletPrompt builds the per-chunk extraction prompt. Two examples
// ground the LLM in the format; the third slot is the chunk to extract from.
func buildChunkTripletPrompt(chunkText string) string {
	return fmt.Sprintf(`Extract informative triplets from the text following the examples.
Make sure the triplet texts are only directly from the given text! Complete directly and strictly following the instructions without any additional words, line break nor space!

Text: Scott Derrickson (born July 16, 1966) is an American director, screenwriter and producer.
Triplets:<Scott Derrickson##born in##1966>$$<Scott Derrickson##nationality##America>$$<Scott Derrickson##occupation##director>$$<Scott Derrickson##occupation##screenwriter>$$<Scott Derrickson##occupation##producer>

Text: A Kiss for Corliss is a 1949 American comedy film directed by Richard Wallace and written by Howard Dimsdale. It stars Shirley Temple in her final starring role as well as her final film appearance.
Triplets:<A Kiss for Corliss##cast member##Shirley Temple>$$<Shirley Temple##served as##Chief of Protocol>

Text: %s
Triplets:`, chunkText)
}

// parseChunkTripletOutput extracts the (h, r, t) triples from the LLM's raw
// response. The response is expected to contain `<h##r##t>` segments separated
// by `$$` and/or whitespace. We use a permissive regex match — we don't
// require the LLM to be perfectly formatted.
//
// Filters (from the KG²RAG reference):
//   - Skip nulls / "no" / "unknown" / "null" / "NULL" placeholders in h or t
//   - Skip self-loops (h == t, after normalization)
//   - Skip if neither h nor t is in the chunk text (LLM hallucination guard)
//
// Weight defaults to 1.0; we don't currently ask the LLM for confidence.
func parseChunkTripletOutput(resp string) []ChunkTriplet {
	out := []ChunkTriplet{}
	matches := chunkTripletRE.FindAllStringSubmatch(resp, -1)
	seen := make(map[string]bool) // dedup by (h, r, t) tuple

	for _, m := range matches {
		h := strings.ToLower(strings.TrimSpace(m[1]))
		r := strings.TrimSpace(m[2])
		t := strings.ToLower(strings.TrimSpace(m[3]))

		// Filter nulls / "no" / "unknown" placeholders
		if isPlaceholder(h) || isPlaceholder(t) || isNoRel(r) {
			continue
		}
		// Skip self-loops
		if h == t {
			continue
		}
		// Skip empty fields
		if h == "" || r == "" || t == "" {
			continue
		}
		// Dedup
		key := h + "##" + r + "##" + t
		if seen[key] {
			continue
		}
		seen[key] = true

		out = append(out, ChunkTriplet{H: h, R: r, T: t, Weight: 1.0})
	}

	if len(out) == 0 {
		slog.Debug("parseChunkTripletOutput: no triplets found", "response_prefix", snippet(resp, 80))
	}
	return out
}

func isPlaceholder(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "no", "unknown", "null", "no ", "unknown ", "null ":
		return true
	}
	return false
}

func isNoRel(r string) bool {
	r = strings.ToLower(strings.TrimSpace(r))
	return r == "no" || r == "unknown" || r == "null"
}

func snippet(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
