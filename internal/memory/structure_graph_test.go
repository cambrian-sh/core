package memory

import (
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// A small parsed document: doc root → Chapter 3 (sec) → Section 3.2 (sec) with
// two leaf paragraphs; Chapter 4 (sec) with one leaf.
func sampleStructuredDoc() *StructuredDocument {
	return &StructuredDocument{
		DocID: "bio", Title: "Bio", Backend: "markdown", OK: true,
		Nodes: []StructNode{
			{ID: "doc", Kind: "document", Level: 0, Title: "Bio"},
			{ID: "sec:n0", Kind: "section", Level: 2, Title: "Chapter 3", ParentID: "doc", Order: 0, Path: "n0", SectionPath: "Chapter 3"},
			{ID: "sec:n0.n0", Kind: "section", Level: 3, Title: "Section 3.2", ParentID: "sec:n0", Order: 0, Path: "n0.n0", SectionPath: "Chapter 3 › Section 3.2"},
			{ID: "chk:n0.n0.n0", Kind: "paragraph", Level: 3, Text: "photosynthesis", ParentID: "sec:n0.n0", Order: 0, Path: "n0.n0.n0", SectionPath: "Chapter 3 › Section 3.2"},
			{ID: "chk:n0.n0.n1", Kind: "paragraph", Level: 3, Text: "calvin cycle", ParentID: "sec:n0.n0", Order: 1, Path: "n0.n0.n1", SectionPath: "Chapter 3 › Section 3.2"},
			{ID: "sec:n1", Kind: "section", Level: 2, Title: "Chapter 4", ParentID: "doc", Order: 1, Path: "n1", SectionPath: "Chapter 4"},
			{ID: "chk:n1.n0", Kind: "paragraph", Level: 2, Text: "genetics", ParentID: "sec:n1", Order: 0, Path: "n1.n0", SectionPath: "Chapter 4"},
		},
	}
}

func TestBuildStructureGraph(t *testing.T) {
	doc := sampleStructuredDoc()
	chunkIDs := []string{"bio-chunk-1", "bio-chunk-2", "bio-chunk-3"} // aligned to 3 ordered leaves
	sections, stamps, edges := BuildStructureGraph(doc, "bio", chunkIDs, nil)

	// 3 section rows, ids namespaced by document.
	if len(sections) != 3 {
		t.Fatalf("want 3 sections, got %d", len(sections))
	}
	byID := map[string]SectionRow{}
	for _, s := range sections {
		byID[s.ID] = s
	}
	s32, ok := byID["bio::sec:n0.n0"]
	if !ok || s32.SectionPath != "Chapter 3 › Section 3.2" || s32.SectionLtree != "n0.n0" || s32.ParentSectionID != "bio::sec:n0" {
		t.Fatalf("section 3.2 row wrong: %+v", s32)
	}

	// Chunk stamps inherit their section path (the key property).
	stampByChunk := map[string]ChunkStamp{}
	for _, s := range stamps {
		stampByChunk[s.ChunkID] = s
	}
	if stampByChunk["bio-chunk-1"].SectionPath != "Chapter 3 › Section 3.2" ||
		stampByChunk["bio-chunk-1"].SectionLtree != "n0.n0.n0" ||
		stampByChunk["bio-chunk-1"].ParentSectionID != "bio::sec:n0.n0" {
		t.Fatalf("chunk-1 stamp wrong: %+v", stampByChunk["bio-chunk-1"])
	}
	if stampByChunk["bio-chunk-3"].SectionPath != "Chapter 4" {
		t.Fatalf("chunk-3 should inherit Chapter 4, got %q", stampByChunk["bio-chunk-3"].SectionPath)
	}

	// Edge assertions: 3.2 PART_OF chapter3; chapter3 NEXT chapter4; chunk-1 PART_OF 3.2.
	has := func(src, tgt string, et domain.EdgeType) bool {
		for _, e := range edges {
			if e.SourceID == src && e.TargetID == tgt && e.Type == et {
				return true
			}
		}
		return false
	}
	if !has("bio::sec:n0.n0", "bio::sec:n0", domain.EdgePartOf) {
		t.Fatalf("missing section PART_OF edge")
	}
	if !has("bio::sec:n0", "bio::sec:n1", domain.EdgeNext) {
		t.Fatalf("missing sibling NEXT edge")
	}
	if !has("bio-chunk-1", "bio::sec:n0.n0", domain.EdgePartOf) {
		t.Fatalf("missing chunk PART_OF section edge")
	}
}

func TestBuildStructureGraph_FlatDocNoSections(t *testing.T) {
	// A doc with no headings → leaves under root, no sections. Should produce no
	// section rows and no stamps (nothing to inherit), and not panic.
	doc := &StructuredDocument{
		DocID: "flat", Backend: "text", OK: true,
		Nodes: []StructNode{
			{ID: "doc", Kind: "document", Level: 0},
			{ID: "chk:n0", Kind: "paragraph", Text: "a", ParentID: "doc", Order: 0, Path: "n0"},
			{ID: "chk:n1", Kind: "paragraph", Text: "b", ParentID: "doc", Order: 1, Path: "n1"},
		},
	}
	sections, stamps, edges := BuildStructureGraph(doc, "flat", []string{"flat-chunk-1", "flat-chunk-2"}, nil)
	if len(sections) != 0 || len(stamps) != 0 || len(edges) != 0 {
		t.Fatalf("flat doc should yield empty graph; got %d/%d/%d", len(sections), len(stamps), len(edges))
	}
}
