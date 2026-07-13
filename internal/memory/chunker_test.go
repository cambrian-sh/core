package memory

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

func testDoc(body string) domain.ExternalDocument {
	return domain.ExternalDocument{
		SourceURI:  "https://example.com/doc.md",
		SourceType: "file_drop",
		Title:      "Test Doc",
		Author:     "alice",
		Body:       body,
		Timestamp:  time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
	}
}

// Cycle 1 — tracer bullet: two paragraphs produce two chunks.
func TestChunkDocument_TwoParagraphs_TwoChunks(t *testing.T) {
	doc := testDoc("First paragraph.\n\nSecond paragraph.")
	chunks := ChunkDocument(doc)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}
	if chunks[0].Body != "First paragraph." {
		t.Errorf("chunk 0 body: got %q", chunks[0].Body)
	}
	if chunks[1].Body != "Second paragraph." {
		t.Errorf("chunk 1 body: got %q", chunks[1].Body)
	}
}

// Cycle 2 — body with no paragraph separator produces one chunk.
func TestChunkDocument_NoParagraphBreak_OneChunk(t *testing.T) {
	doc := testDoc("Single paragraph with no double newline.")
	chunks := ChunkDocument(doc)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if chunks[0].Body != "Single paragraph with no double newline." {
		t.Errorf("chunk 0 body: got %q", chunks[0].Body)
	}
}

// Cycle 3 — paragraph exceeding maxChunkChars is split at sentence boundaries.
func TestChunkDocument_LongParagraph_SentenceSplit(t *testing.T) {
	// Build a paragraph longer than 1000 chars using short sentences.
	var sb strings.Builder
	for sb.Len() < 1100 {
		sb.WriteString("This is a sentence. ")
	}
	longPara := strings.TrimSpace(sb.String())

	doc := testDoc(longPara)
	chunks := ChunkDocument(doc)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple sentence chunks from long paragraph, got %d", len(chunks))
	}
	for i, ch := range chunks {
		if len(ch.Body) > 1000 {
			t.Errorf("chunk %d exceeds maxChunkChars: len=%d", i, len(ch.Body))
		}
	}
}

// Cycle 4 — every chunk carries the required provenance metadata.
func TestChunkDocument_MetadataPropagated(t *testing.T) {
	doc := testDoc("Para one.\n\nPara two.")
	chunks := ChunkDocument(doc)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}
	for i, ch := range chunks {
		if ch.Metadata["chunk_index"] != i {
			t.Errorf("chunk %d: chunk_index=%v want %d", i, ch.Metadata["chunk_index"], i)
		}
		if ch.Metadata["total_chunks"] != 2 {
			t.Errorf("chunk %d: total_chunks=%v want 2", i, ch.Metadata["total_chunks"])
		}
		if ch.Metadata["source_uri"] != doc.SourceURI {
			t.Errorf("chunk %d: source_uri=%v", i, ch.Metadata["source_uri"])
		}
		if ch.Metadata["source_type"] != doc.SourceType {
			t.Errorf("chunk %d: source_type=%v", i, ch.Metadata["source_type"])
		}
		if ch.Metadata["author"] != doc.Author {
			t.Errorf("chunk %d: author=%v", i, ch.Metadata["author"])
		}
		if ch.Metadata["timestamp"] != doc.Timestamp.Format("2006-01-02T15:04:05Z07:00") {
			t.Errorf("chunk %d: timestamp=%v", i, ch.Metadata["timestamp"])
		}
	}
}

// Cycle 5 — buildBodyPreview returns first paragraph when ≤150 chars.
func TestBuildBodyPreview_ShortFirstParagraph(t *testing.T) {
	body := "Short first paragraph.\n\nThis should not appear."
	got := buildBodyPreview(body, 150)
	if got != "Short first paragraph." {
		t.Errorf("got %q", got)
	}
}

// Cycle 6 — buildBodyPreview truncates first paragraph at word boundary.
func TestBuildBodyPreview_LongFirstParagraph_WordBoundaryTruncation(t *testing.T) {
	// Build a first paragraph definitively longer than 150 chars.
	first := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 5) // 225 chars
	body := first + "\n\nShould not appear."

	got := buildBodyPreview(body, 150)

	if len(got) > 153 { // 150 + "..."
		t.Errorf("preview too long: len=%d got=%q", len(got), got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("expected truncated preview to end with '...', got %q", got)
	}
	// Must not cut mid-word.
	withoutEllipsis := strings.TrimSuffix(got, "...")
	lastChar := withoutEllipsis[len(withoutEllipsis)-1]
	if lastChar == ' ' {
		t.Errorf("preview should not end with a trailing space before '...'")
	}
}

// Cycle 7 — buildBodyPreview falls back to word-boundary truncation when no \n\n.
func TestBuildBodyPreview_NoSeparator_FallbackTruncation(t *testing.T) {
	long := strings.Repeat("word ", 100) // 500 chars, all one paragraph
	got := buildBodyPreview(long, 150)
	if len(got) > 153 {
		t.Errorf("preview too long: len=%d", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("expected '...' suffix, got %q", got)
	}
}

// Cycle 8 (T-1.12) — the T-1.4 refactor moved the legacy ChunkDocument
// body into OptionCChunker. ChunkDocument is now a shim that calls
// OptionCChunker.Chunk(ctx, &doc). The registry routes any (sourceType,
// ext) without a matching route to the configured default chunker, and
// the spec default is "option_c". This test pins the equivalence: the
// legacy ChunkDocument output must equal the registry-resolved
// OptionCChunker output, element-wise on Body and on the six required
// metadata keys. If anyone changes OptionC's behavior (e.g. adds a new
// metadata key, drops a key, changes the chunk boundary logic), this
// test fails — the regression bar for T-1.4 / T-1.8 / T-1.12.
func TestChunkRegistry_OptionC_MatchesChunkDocument(t *testing.T) {
	doc := testDoc("First paragraph.\n\nSecond paragraph.")

	reg, err := NewRegistry(defaultChunkers(), ChunkerConfig{Default: "option_c"})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	chunker, err := reg.Resolve("file_drop", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got, want := chunker.Name(), "option_c"; got != want {
		t.Fatalf("resolved chunker name = %q, want %q", got, want)
	}

	ctx := context.Background()
	newChunks, err := chunker.Chunk(ctx, &doc)
	if err != nil {
		t.Fatalf("chunker.Chunk: %v", err)
	}
	oldChunks := ChunkDocument(doc)

	if len(newChunks) != len(oldChunks) {
		t.Fatalf("chunk count: new=%d old=%d", len(newChunks), len(oldChunks))
	}

	requiredKeys := []string{
		"chunk_index",
		"total_chunks",
		"source_uri",
		"source_type",
		"author",
		"timestamp",
	}

	for i := range newChunks {
		if newChunks[i].Body != oldChunks[i].Body {
			t.Errorf("chunk %d: Body mismatch: new=%q old=%q", i, newChunks[i].Body, oldChunks[i].Body)
		}
		for _, k := range requiredKeys {
			if newChunks[i].Metadata[k] != oldChunks[i].Metadata[k] {
				t.Errorf("chunk %d: Metadata[%q] mismatch: new=%v old=%v",
					i, k, newChunks[i].Metadata[k], oldChunks[i].Metadata[k])
			}
		}
	}
}
