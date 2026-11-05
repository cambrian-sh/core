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
		out = appendChunkTriplet(out, seen, m[1], m[2], m[3])
	}
	if len(out) == 0 {
		slog.Debug("parseChunkTripletOutput: no triplets found", "response_prefix", snippet(resp, 80))
	}
	return out
}

// appendChunkTriplet applies the KG²RAG triplet filters to one raw (h, r, t) and
// appends it when it survives. It is the ONE normalisation both LLM reply formats —
// the legacy <h##r##t> completion and the structured JSON reply — go through, so the
// format change cannot change which triplets reach chunk_triplets:
//   - h and t are lower-cased and trimmed, r is trimmed;
//   - null / "no" / "unknown" placeholders in h or t, and "no"/"unknown"/"null"
//     relations, are skipped;
//   - self-loops (h == t) and empty fields are skipped;
//   - duplicates (by h##r##t) are skipped.
func appendChunkTriplet(out []domain.ChunkTriplet, seen map[string]bool, rawH, rawR, rawT string) []domain.ChunkTriplet {
	h := strings.ToLower(strings.TrimSpace(rawH))
	r := strings.TrimSpace(rawR)
	t := strings.ToLower(strings.TrimSpace(rawT))
	if isPlaceholder(h) || isPlaceholder(t) || isNoRel(r) {
		return out
	}
	if h == t {
		return out
	}
	if h == "" || r == "" || t == "" {
		return out
	}
	key := h + "##" + r + "##" + t
	if seen[key] {
		return out
	}
	seen[key] = true
	return append(out, domain.ChunkTriplet{H: h, R: r, T: t, Weight: 1.0})
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
