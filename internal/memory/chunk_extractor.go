package memory

import (
	"log/slog"
	"regexp"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// chunkTripletRE matches the LLM's triplet output format: <h##r##t>.
// The LLM is prompted to emit triplets separated by `$$`.
var chunkTripletRE = regexp.MustCompile(`<([^<>]+)##([^<>]+)##([^<>]+)>`)

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
func parseChunkTripletOutput(resp string) []domain.ChunkTriplet {
	out := []domain.ChunkTriplet{}
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

		out = append(out, domain.ChunkTriplet{H: h, R: r, T: t, Weight: 1.0})
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
