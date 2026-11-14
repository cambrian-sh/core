package memory

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// ENG-485 regression: a single oversized leaf (a 70 KB conversation with
// single-newline turns, as the docling sidecar returned it) must become several
// retrieval-sized chunks, not one 70 KB chunk, and every piece keeps the leaf's
// section stamp and positional rep alignment.
func TestChunksFromLeaves_SplitsAnOversizedLeaf(t *testing.T) {
	var turns []string
	for i := 0; i < 700; i++ {
		turns = append(turns, "user: I baked a croissant today and it came out flaky and golden, number "+strings.Repeat("x", 1+i%7))
	}
	big := strings.Join(turns, "\n")
	doc := &StructuredDocument{Nodes: []StructNode{
		{ID: "s1", Kind: structNodeKindSection, Title: "Session", SectionPath: "", Path: "n0", Order: 0},
		{ID: "l1", Kind: "paragraph", Text: big, ParentID: "s1", SectionPath: "Session", Path: "n0.n0", Order: 0},
		{ID: "l2", Kind: "paragraph", Text: "short tail", ParentID: "s1", SectionPath: "Session", Path: "n0.n1", Order: 1},
	}}
	chunks, reps := ChunksFromLeaves(doc)
	if len(chunks) < len(big)/maxChunkChars {
		t.Fatalf("got %d chunks for a %d-char leaf; the oversized leaf was not split", len(chunks), len(big))
	}
	if len(chunks) != len(reps) {
		t.Fatalf("chunks/reps misaligned: %d vs %d", len(chunks), len(reps))
	}
	var rebuilt []string
	for i, c := range chunks {
		if len(c.Body) > maxChunkChars {
			t.Fatalf("chunk %d is %d chars, over %d", i, len(c.Body), maxChunkChars)
		}
		if i < len(chunks)-1 {
			if reps[i].ID != "l1" || c.Metadata["section_path"] != "Session" {
				t.Fatalf("piece %d lost its leaf/section: rep=%s meta=%v", i, reps[i].ID, c.Metadata)
			}
			rebuilt = append(rebuilt, c.Body)
		}
	}
	if strings.Join(rebuilt, "\n") != big {
		t.Fatal("split pieces do not reassemble the leaf text (content lost or reordered)")
	}
	// the short leaf after an oversized one is its own chunk, never merged into a piece
	if last := chunks[len(chunks)-1]; last.Body != "short tail" || reps[len(reps)-1].ID != "l2" {
		t.Fatalf("tail leaf = %q (rep %s)", last.Body, reps[len(reps)-1].ID)
	}
}

// Small leaves still merge exactly as before (no behaviour change below the cap).
func TestChunksFromLeaves_SmallLeavesStillMerge(t *testing.T) {
	doc := &StructuredDocument{Nodes: []StructNode{
		{ID: "s1", Kind: structNodeKindSection, Title: "S", Path: "n0"},
		{ID: "a", Kind: "paragraph", Text: "alpha", ParentID: "s1", SectionPath: "S", Path: "n0.n0", Order: 0},
		{ID: "b", Kind: "paragraph", Text: "beta", ParentID: "s1", SectionPath: "S", Path: "n0.n1", Order: 1},
	}}
	chunks, reps := ChunksFromLeaves(doc)
	if len(chunks) != 1 || chunks[0].Body != "alpha\nbeta" || reps[0].ID != "a" {
		t.Fatalf("got %+v", chunks)
	}
}

// A line with no line or sentence boundary is hard-cut, but never inside a rune.
func TestSplitOversizedLeaf_HardCutKeepsUTF8Valid(t *testing.T) {
	text := strings.Repeat("ğüşıöç", 400) // 2-byte runes, no spaces
	for i, p := range splitOversizedLeaf(text, maxChunkChars) {
		if !utf8.ValidString(p) {
			t.Fatalf("piece %d is not valid UTF-8", i)
		}
		if len(p) > maxChunkChars {
			t.Fatalf("piece %d is %d bytes", i, len(p))
		}
	}
}
