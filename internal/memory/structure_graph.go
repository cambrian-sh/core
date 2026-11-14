package memory

import (
	"context"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// Document-structure graph (ADR-0060). The docling_agent parses a document into a
// normalized hierarchy (sections + leaf content, each leaf carrying its section
// breadcrumb + an ltree ordinal path). This file turns that tree into rows the
// store persists: section nodes (documents rows, document_type=doc_section),
// structural edges (PART_OF / NEXT), and per-chunk stamps (section_path +
// section_ltree + parent_section_id) — the "inherit the structural path onto
// every leaf" step that lets retrieval filter/expand by location.

// ChunksFromLeaves turns a parsed document's ordered leaves into the chunk set.
// With structure_graph_enabled the leaves ARE the chunks, so chunk boundaries
// match the hierarchy and each chunk's section stamp is exact. Small consecutive
// leaves of one section are merged up to defaultChunkMergeTarget; a leaf LONGER
// than maxChunkChars is split (splitOversizedLeaf) into several chunks that all
// inherit that leaf's section. The second return value is the representative leaf
// of each chunk, positionally aligned with the chunks (BuildStructureGraph relies
// on chunk_i <-> reps_i). Returns nil for no leaves.
//
// Why split: the merge loop never split, so a sidecar that returned one text leaf
// for a whole document (a 70 KB conversation with single-newline turns) stored ONE
// 70,230-char chunk. The embedder reads only its first ~512 tokens, so the rest was
// invisible to dense retrieval and one hit carried 70 KB
// (T-20261103-structure-aware-ingestion-stores-an-oversized). The flat default
// (OptionCChunker) splits at the same maxChunkChars.
const defaultChunkMergeTarget = 500 // chars: merge tiny leaves up to ~this size

func ChunksFromLeaves(doc *StructuredDocument) ([]domain.Chunk, []StructNode) {
	if doc == nil {
		return nil, nil
	}
	leaves := doc.OrderedLeaves()
	var chunks []domain.Chunk
	var reps []StructNode // representative (first) leaf per chunk — drives stamping
	add := func(rep StructNode, body string) {
		chunks = append(chunks, domain.Chunk{
			Body:     body,
			Metadata: map[string]any{"struct_kind": rep.Kind, "section_path": rep.SectionPath},
		})
		reps = append(reps, rep)
	}
	i := 0
	for i < len(leaves) {
		rep := leaves[i]
		if len(rep.Text) > maxChunkChars {
			for _, piece := range splitOversizedLeaf(rep.Text, maxChunkChars) {
				add(rep, piece)
			}
			i++
			continue
		}
		var b strings.Builder
		b.WriteString(rep.Text)
		j := i + 1
		// Merge following leaves that live in the SAME section, until the target
		// size — so chunks are retrieval-sized without crossing a section boundary
		// (keeps each merged chunk's inherited section path unambiguous). An
		// oversized leaf is never merged in: it is split on its own turn.
		for j < len(leaves) && leaves[j].ParentID == rep.ParentID && b.Len() < defaultChunkMergeTarget &&
			len(leaves[j].Text) <= maxChunkChars {
			b.WriteByte(10)
			b.WriteString(leaves[j].Text)
			j++
		}
		add(rep, b.String())
		i = j
	}
	return chunks, reps
}

// splitOversizedLeaf cuts one leaf's text into pieces of at most max bytes,
// preferring line boundaries (conversation turns, list items) and packing whole
// lines up to max; a single line longer than max falls back to OptionC's sentence
// splitter. Every piece is non-empty and trimmed.
func splitOversizedLeaf(text string, max int) []string {
	var out []string
	var b strings.Builder
	flush := func() {
		if t := strings.TrimSpace(b.String()); t != "" {
			out = append(out, t)
		}
		b.Reset()
	}
	for _, line := range strings.Split(text, "\n") {
		if len(line) > max {
			flush()
			out = append(out, splitBySentence(strings.TrimSpace(line))...)
			continue
		}
		if b.Len() > 0 && b.Len()+1+len(line) > max {
			flush()
		}
		if b.Len() > 0 {
			b.WriteByte(10)
		}
		b.WriteString(line)
	}
	flush()
	return out
}

// StructNode mirrors the sidecar's node JSON (agents/system/docling_agent/structure.py).
type StructNode struct {
	ID          string         `json:"id"`
	Kind        string         `json:"kind"`
	Level       int            `json:"level"`
	Title       string         `json:"title"`
	Text        string         `json:"text"`
	ParentID    string         `json:"parent_id"`
	Order       int            `json:"order"`
	Path        string         `json:"path"`         // ltree-safe ordinal path, e.g. "n0.n2.n1"
	SectionPath string         `json:"section_path"` // human breadcrumb of ancestor section titles
	Page        *int           `json:"page"`
	Meta        map[string]any `json:"meta"`
}

// StructuredDocument mirrors the sidecar's document JSON.
type StructuredDocument struct {
	DocID      string       `json:"doc_id"`
	Title      string       `json:"title"`
	SourceType string       `json:"source_type"`
	Backend    string       `json:"backend"`
	Nodes      []StructNode `json:"nodes"`
	OK         bool         `json:"ok"`
	// DroppedLeaves counts content leaves the sidecar's junk filter deleted
	// before they could become chunks. Since junk_mode "mark" the sidecar drops
	// nothing and this stays 0; it is non-zero only from a legacy sidecar, and
	// applyStructureJunkFilter still reports and counts it.
	DroppedLeaves int `json:"dropped_leaves"`
	// ParseMs is the sidecar's own processing time for this document. Diagnostic
	// only: the ingest timing line subtracts it from the dispatch round-trip to
	// separate queue wait + IPC from parsing. 0 from a sidecar that predates it.
	ParseMs float64 `json:"parse_ms,omitempty"`
}

const structNodeKindSection = "section"
const structNodeKindDocument = "document"

// OrderedLeaves returns the non-section, non-document nodes in document order —
// the retrievable content units, aligned positionally with the ingested chunks.
func (d *StructuredDocument) OrderedLeaves() []StructNode {
	out := make([]StructNode, 0, len(d.Nodes))
	for _, n := range d.Nodes {
		if n.Kind != structNodeKindSection && n.Kind != structNodeKindDocument {
			out = append(out, n)
		}
	}
	return out
}

func (d *StructuredDocument) sections() []StructNode {
	out := make([]StructNode, 0)
	for _, n := range d.Nodes {
		if n.Kind == structNodeKindSection {
			out = append(out, n)
		}
	}
	return out
}

// StructureParser is the port the ingest path uses to turn a document into its
// hierarchy. The DoclingDispatcher (internal/substrate/network) implements it by
// calling the docling_agent; tests use a fake.
type StructureParser interface {
	Parse(ctx context.Context, req StructureParseRequest) (*StructuredDocument, error)
}

// StructureParseRequest is the sidecar request. Either Text (markdown/plain) or
// DataB64 (raw bytes for Docling) is set; SourceType routes the backend.
type StructureParseRequest struct {
	DocID      string
	Title      string
	SourceType string
	Text       string
	DataB64    string
	// JunkMode "mark" makes the sidecar keep every leaf and tag OCR/layout junk
	// with meta.junk_reason, so the kernel decides what to drop (structure_junk.go).
	// Empty = the sidecar's legacy in-process drop.
	JunkMode string
}

// ── persisted primitives ─────────────────────────────────────────────────────

// The persisted structure-graph types and the StructureGraphStore port live in
// domain/memory_ports.go (owner decision 2026-11-14, ENG-1104: the postgres adapter
// must not import this service package). Aliases keep this package's callers and the
// operator plane compiling unchanged.
type (
	SectionRow          = domain.SectionRow
	ChunkStamp          = domain.ChunkStamp
	StructuralEdge      = domain.StructuralEdge
	StructureGraphStore = domain.StructureGraphStore
)

// BuildStructureGraph is the pure assembly step: given a parsed StructuredDocument,
// the source document id, and the ingested chunk ids in reading order, it returns
// the section rows, per-chunk stamps, and structural edges to persist.
//
// orderedChunkIDs is aligned positionally with doc.OrderedLeaves(): chunk i
// inherits the section path of leaf i. When the counts differ (the chunker and
// the parser disagreed on boundaries) only the aligned prefix is stamped; the
// section graph is still fully persisted. Pure + deterministic → unit-testable.
func BuildStructureGraph(doc *StructuredDocument, documentID string, orderedChunkIDs []string, chunkLeaves []StructNode) (
	sections []SectionRow, stamps []ChunkStamp, edges []StructuralEdge) {
	if doc == nil {
		return nil, nil, nil
	}
	rowID := func(nodeID string) string { return documentID + "::" + nodeID }

	// Section rows + PART_OF(parent) edges. Parent is a section row when the
	// node's parent is a section; otherwise the section sits under the document
	// root and gets no PART_OF (it is a top-level section of the document).
	byID := make(map[string]StructNode, len(doc.Nodes))
	for _, n := range doc.Nodes {
		byID[n.ID] = n
	}
	for _, s := range doc.sections() {
		parentRow := ""
		if p, ok := byID[s.ParentID]; ok && p.Kind == structNodeKindSection {
			parentRow = rowID(p.ID)
		}
		sections = append(sections, SectionRow{
			ID: rowID(s.ID), DocumentID: documentID, Title: s.Title, Level: s.Level,
			Order: s.Order, SectionPath: s.SectionPath, SectionLtree: s.Path,
			ParentSectionID: parentRow,
		})
		if parentRow != "" {
			edges = append(edges, StructuralEdge{SourceID: rowID(s.ID), TargetID: parentRow, Type: domain.EdgePartOf})
		}
	}

	// NEXT edges between sibling sections (same parent, consecutive order).
	prevAtParent := make(map[string]string) // parentID -> previous section row id
	for _, s := range doc.sections() {
		if prev, ok := prevAtParent[s.ParentID]; ok {
			edges = append(edges, StructuralEdge{SourceID: prev, TargetID: rowID(s.ID), Type: domain.EdgeNext})
		}
		prevAtParent[s.ParentID] = rowID(s.ID)
	}

	// Chunk stamps + chunk PART_OF section edges, aligned to the per-chunk
	// representative leaves (from ChunksFromLeaves' merge); falls back to every
	// ordered leaf when no explicit alignment is given (1 leaf == 1 chunk).
	leaves := chunkLeaves
	if leaves == nil {
		leaves = doc.OrderedLeaves()
	}
	n := len(leaves)
	if len(orderedChunkIDs) < n {
		n = len(orderedChunkIDs)
	}
	for i := 0; i < n; i++ {
		leaf := leaves[i]
		cid := orderedChunkIDs[i]
		parentRow := ""
		if p, ok := byID[leaf.ParentID]; ok && p.Kind == structNodeKindSection {
			parentRow = rowID(p.ID)
		}
		// A leaf under only the document root has no section — skip stamping it
		// with an empty path (nothing to inherit), but it's harmless either way.
		if parentRow == "" && strings.TrimSpace(leaf.SectionPath) == "" {
			continue
		}
		stamps = append(stamps, ChunkStamp{
			ChunkID: cid, SectionPath: leaf.SectionPath, SectionLtree: leaf.Path,
			ParentSectionID: parentRow,
		})
		if parentRow != "" {
			edges = append(edges, StructuralEdge{SourceID: cid, TargetID: parentRow, Type: domain.EdgePartOf})
		}
	}
	return sections, stamps, edges
}
