// Package memory — Markdown Header Chunker.
//
// MarkdownHeaderChunker splits Markdown text on header lines (# to
// ######), retaining the header hierarchy in each chunk's
// Metadata["section_path"] (ADR-0060 D2, T-1.6). The shape mirrors
// LangChain's MarkdownHeaderTextSplitter: each chunk's body is
// "<header>\n\n<content>" (just "<content>" for the pre-header
// preamble), and the section_path carries the parent header chain
// from H1 down to the chunk's own header level.
//
// Supports only .md; the registry's fall-through handles the rest.
package memory

import (
	"context"
	"regexp"

	"github.com/cambrian-sh/core/domain"
)

// markdownHeaderRe matches lines starting with one to six '#'
// characters followed by whitespace and the header text.
var markdownHeaderRe = regexp.MustCompile(`^(#{1,6})\s+(.*?)\s*$`)

// MarkdownHeaderChunker is a zero-value-friendly domain.Chunker.
// No per-call tunables.
type MarkdownHeaderChunker struct{}

// Name is the registry key. Stable.
func (MarkdownHeaderChunker) Name() string { return "markdown_header" }

// Supports returns true only for ".md".
func (MarkdownHeaderChunker) Supports(sourceType, ext string) bool {
	_ = sourceType
	return ext == ".md"
}

// Chunk splits body on Markdown header lines. Each chunk's body
// starts with its header line and the content lines that follow
// until the next header. The section_path metadata carries the
// parent header chain (e.g. ["# Top", "## Section"] for a chunk
// under ## Section).
//
// Returns []Chunk{} for an empty or whitespace-only body. Returns
// one chunk with body == input and section_path == [] for a body
// without any header lines (the entire doc is the preamble).
func (m MarkdownHeaderChunker) Chunk(ctx context.Context, doc *domain.ExternalDocument) ([]domain.Chunk, error) {
	_ = ctx
	if doc == nil {
		return nil, nil
	}
	body := doc.Body
	if trimIsEmpty(body) {
		return []domain.Chunk{}, nil
	}

	type pending struct {
		header   string
		path     []string
		content  []string
	}
	var (
		pendingChunks []pending
		current       = pending{path: []string{}}
	)
	flush := func() {
		if current.header == "" && len(current.content) == 0 {
			return
		}
		pendingChunks = append(pendingChunks, current)
		current = pending{path: []string{}}
	}

	lines := splitLinesKeepEmpty(body)
	for _, line := range lines {
		match := markdownHeaderRe.FindStringSubmatch(line)
		if match == nil {
			current.content = append(current.content, line)
			continue
		}
		// Cut at the previous chunk; the new chunk starts at this
		// header. Build the new path by keeping the previous
		// ancestors strictly above the new header's level.
		prevPath := append([]string{}, current.path...)
		flush()
		level := len(match[1])
		headerText := match[1] + " " + match[2]
		newPath := make([]string, 0, level)
		for _, h := range prevPath {
			if headerLevel(h) < level {
				newPath = append(newPath, h)
			}
		}
		newPath = append(newPath, headerText)
		current = pending{header: headerText, path: newPath, content: []string{}}
	}
	flush()

	total := len(pendingChunks)
	out := make([]domain.Chunk, total)
	for i, p := range pendingChunks {
		out[i] = domain.Chunk{
			Body:    joinChunkBody(p.header, p.content),
			Metadata: map[string]any{
				"chunk_index":  i,
				"total_chunks": total,
				"section_path": append([]string{}, p.path...),
			},
		}
	}
	return out, nil
}

// headerLevel returns the '#' count at the start of headerText, or
// 0 if it does not look like a Markdown header.
func headerLevel(headerText string) int {
	match := markdownHeaderRe.FindStringSubmatch(headerText)
	if match == nil {
		return 0
	}
	return len(match[1])
}

// joinChunkBody produces "<header>\n\n<content>" or just
// "<content>" if header is empty.
func joinChunkBody(header string, content []string) string {
	contentStr := joinTrimmedLines(content)
	if contentStr == "" {
		return header
	}
	if header == "" {
		return contentStr
	}
	return header + "\n\n" + contentStr
}

// trimIsEmpty is true if s is empty or contains only whitespace.
func trimIsEmpty(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			return false
		}
	}
	return true
}
