// Package memory — Option C Chunker.
//
// OptionCChunker is the back-compat default chunker (ADR-0060 D2, T-1.4).
// It is extracted verbatim from the legacy 115-line ChunkDocument: the
// only behavioural change vs the old shim is that the body+metadata
// shape now lives in a domain.Chunk value (the port contract). The
// extracted logic is unchanged.
//
// Pipeline:
//  1. splitParagraphs: split body on blank-line ("\n\n") separators;
//     drop empty parts; the empty-body case yields one chunk with body
//     "" (the back-compat floor that callers depend on).
//  2. For each paragraph <= maxChunkChars (1000), the paragraph is
//     emitted verbatim.
//  3. For each paragraph > maxChunkChars, splitBySentence cuts at the
//     last sentence boundary (". ", "! ", "? ") within the first
//     maxChunkChars, then recurses on the remainder.
//
// Each chunk's Metadata carries the 6 provenance keys the test
// TestChunkDocument_MetadataPropagated pins:
//   - chunk_index (0..N-1)
//   - total_chunks (N)
//   - source_uri  (the ExternalDocument's SourceURI)
//   - source_type (SourceType: "file_drop", "slack", etc.)
//   - author      (Author)
//   - timestamp   (Timestamp formatted RFC3339)
package memory

import (
	"context"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// maxChunkChars is the threshold above which a paragraph is split at
// sentence boundaries. Matches the original ChunkDocument constant.
const maxChunkChars = 1000

// previewMaxChars is the upper bound for buildBodyPreview. Used by
// IngestionManager.mintSourceDoc to attach a short body preview to
// the source-doc entity (the snippet arg of ContentStore.Put).
const previewMaxChars = 150

// OptionCChunker is the back-compat default. Empty struct: no per-call
// state. Implements domain.Chunker (Name/Supports/Chunk).
type OptionCChunker struct{}

// Name is the registry key. Stable: renaming is a breaking change to
// the chunker config schema.
func (OptionCChunker) Name() string { return "option_c" }

// Supports returns true for every (sourceType, ext). OptionC is the
// floor — the registry falls through to it when no route matches, and
// the spec deliberately does not gate it on extension or source type.
func (OptionCChunker) Supports(sourceType, ext string) bool {
	_ = sourceType
	_ = ext
	return true
}

// Chunk splits the doc body into Option C chunks with full
// provenance metadata. The ctx arg is unused (Option C is a pure
// function of the body) but required by the Chunker port.
func (o OptionCChunker) Chunk(ctx context.Context, doc *domain.ExternalDocument) ([]domain.Chunk, error) {
	_ = ctx
	if doc == nil {
		return nil, nil
	}
	paragraphs := splitParagraphs(doc.Body)
	var bodies []string
	for _, para := range paragraphs {
		if len(para) <= maxChunkChars {
			bodies = append(bodies, para)
			continue
		}
		bodies = append(bodies, splitBySentence(para)...)
	}
	total := len(bodies)
	timestampStr := ""
	if !doc.Timestamp.IsZero() {
		timestampStr = doc.Timestamp.Format("2006-01-02T15:04:05Z07:00")
	}
	chunks := make([]domain.Chunk, total)
	for i, body := range bodies {
		chunks[i] = domain.Chunk{
			Body: body,
			Metadata: map[string]any{
				"chunk_index": i,
				"total_chunks": total,
				"source_uri": doc.SourceURI,
				"source_type": doc.SourceType,
				"author": doc.Author,
				"timestamp": timestampStr,
			},
		}
	}
	return chunks, nil
}

// splitParagraphs splits body on blank-line ("\n\n") separators,
// trims each part, and drops empty parts. If body has no non-empty
// parts (all whitespace), returns [text.strip()] so callers get
// exactly one chunk with body "" — the back-compat floor the
// document-qa suite and the old ChunkDocument test rely on.
func splitParagraphs(body string) []string {
	parts := strings.Split(body, "\n\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return []string{strings.TrimSpace(body)}
	}
	return out
}

// splitBySentence splits a paragraph longer than maxChunkChars at
// sentence boundaries. Strategy: look in the first maxChunkChars for
// the last occurrence of ". "/"! "/"?\n" (the char after the
// sentence-ending punctuation must be a space or newline). If found,
// cut there. If not, hard-cut at maxChunkChars. Repeat on the
// remainder until remainder <= maxChunkChars.
//
// Mirrors the original ChunkDocument.splitBySentence behaviour
// exactly — the only difference is the Go idioms (no Python range
// reverse scan; we just track the best cut as we go).
func splitBySentence(para string) []string {
	var out []string
	remaining := para
	for len(remaining) > maxChunkChars {
		// Find the last sentence-end within the first maxChunkChars.
		cutAt := -1
		for i := maxChunkChars - 1; i > 0; i-- {
			if i >= len(remaining) {
				continue
			}
			if remaining[i] != ' ' && remaining[i] != '\n' {
				continue
			}
			prev := remaining[i-1]
			if prev == '.' || prev == '!' || prev == '?' {
				cutAt = i
				break
			}
		}
		if cutAt <= 0 {
			cutAt = maxChunkChars
		}
		out = append(out, strings.TrimSpace(remaining[:cutAt]))
		remaining = strings.TrimSpace(remaining[cutAt:])
	}
	if remaining != "" {
		out = append(out, remaining)
	}
	return out
}

// buildBodyPreview returns a short preview of body for the
// source-doc entity's content_cid snippet. The preview is the first
// paragraph (split on "\n\n"), truncated at a word boundary if longer
// than maxChars, with "..." appended when truncated.
//
// Truncation rules:
//   - short first paragraph (<= maxChars) → returned verbatim
//   - long first paragraph → cut at the last ASCII space within the
//     first maxChars; append "..." so the caller can tell it was
//     truncated
//   - body without a "\n\n" separator → fall back to the same
//     word-boundary truncation on the whole body
//   - output length is bounded: <= maxChars + len("...") = maxChars+3
//
// This is the same preview the legacy ChunkDocument emitted; the
// in-tree callers (IngestionManager.mintSourceDoc) read it for the
// source-doc entity's "snippet" column.
func buildBodyPreview(body string, maxChars int) string {
	if maxChars < 0 {
		maxChars = 0
	}
	first := body
	if idx := strings.Index(body, "\n\n"); idx >= 0 {
		first = body[:idx]
	}
	first = strings.TrimSpace(first)
	if len(first) <= maxChars {
		return first
	}
	truncated := first[:maxChars]
	if i := strings.LastIndexByte(truncated, ' '); i > 0 {
		truncated = truncated[:i]
	}
	return truncated + "..."
}
