package memory

import (
	"strings"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

const maxChunkChars = 1000
const previewMaxChars = 150

// Chunk is one embeddable segment produced from an ExternalDocument.
type Chunk struct {
	Body     string
	Metadata map[string]interface{}
}

// ChunkDocument splits doc.Body into paragraph-boundary segments.
// Paragraphs exceeding maxChunkChars are further split at sentence boundaries.
// Every chunk carries full provenance metadata.
func ChunkDocument(doc domain.ExternalDocument) []Chunk {
	paragraphs := splitParagraphs(doc.Body)
	var bodies []string
	for _, para := range paragraphs {
		if len(para) <= maxChunkChars {
			bodies = append(bodies, para)
		} else {
			bodies = append(bodies, splitBySentence(para)...)
		}
	}

	total := len(bodies)
	chunks := make([]Chunk, total)
	for i, body := range bodies {
		chunks[i] = Chunk{
			Body: body,
			Metadata: map[string]interface{}{
				"chunk_index":  i,
				"total_chunks": total,
				"source_uri":   doc.SourceURI,
				"source_type":  doc.SourceType,
				"author":       doc.Author,
				"timestamp":    doc.Timestamp.Format("2006-01-02T15:04:05Z07:00"),
			},
		}
	}
	return chunks
}

// splitParagraphs splits text on double-newline boundaries, trimming whitespace.
func splitParagraphs(text string) []string {
	parts := strings.Split(text, "\n\n")
	var out []string
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		return []string{strings.TrimSpace(text)}
	}
	return out
}

// splitBySentence splits a long paragraph into chunks ≤ maxChunkChars at
// sentence boundaries (`. `, `! `, `? `). Falls back to hard truncation if
// no sentence boundary is found within the limit.
func splitBySentence(para string) []string {
	var chunks []string
	remaining := para
	for len(remaining) > maxChunkChars {
		cutAt := -1
		// Search backwards from maxChunkChars for a sentence boundary.
		for i := maxChunkChars; i > 0; i-- {
			if i < len(remaining) && (remaining[i] == ' ' || remaining[i] == '\n') {
				prev := remaining[i-1]
				if prev == '.' || prev == '!' || prev == '?' {
					cutAt = i
					break
				}
			}
		}
		if cutAt <= 0 {
			// No sentence boundary found: hard cut at maxChunkChars.
			cutAt = maxChunkChars
		}
		chunks = append(chunks, strings.TrimSpace(remaining[:cutAt]))
		remaining = strings.TrimSpace(remaining[cutAt:])
	}
	if remaining != "" {
		chunks = append(chunks, remaining)
	}
	return chunks
}

// buildBodyPreview extracts the first paragraph of body; if it exceeds maxChars,
// it truncates at the last word boundary before the limit and appends "...".
func buildBodyPreview(body string, maxChars int) string {
	// Extract first paragraph.
	para := body
	if idx := strings.Index(body, "\n\n"); idx >= 0 {
		para = strings.TrimSpace(body[:idx])
	}

	if len(para) <= maxChars {
		return para
	}

	// Truncate at last word boundary within maxChars.
	preview := para[:maxChars]
	if i := strings.LastIndexByte(preview, ' '); i > 0 {
		preview = preview[:i]
	}
	return strings.TrimRight(preview, " ") + "..."
}
