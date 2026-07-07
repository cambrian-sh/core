package memory

import (
	"context"
	"strings"

	"github.com/cambrian-sh/cambrian-runtime/domain"
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
// match the hierarchy and each chunk's section stamp is exact (chunk_i == leaf_i).
// One chunk per ordered leaf, preserving order (no filtering) so the positional
// alignment BuildStructureGraph relies on is guaranteed. Returns nil for no leaves.
const defaultChunkMergeTarget = 500 // chars: merge tiny leaves up to ~this size

func ChunksFromLeaves(doc *StructuredDocument) ([]domain.Chunk, []StructNode) {
	if doc == nil {
		return nil, nil
	}
	leaves := doc.OrderedLeaves()
	var chunks []domain.Chunk
	var reps []StructNode // representative (first) leaf per merged chunk — drives stamping
	i := 0
	for i < len(leaves) {
		rep := leaves[i]
		var b strings.Builder
		b.WriteString(rep.Text)
		j := i + 1
		// Merge following leaves that live in the SAME section, until the target
		// size — so chunks are retrieval-sized without crossing a section boundary
		// (keeps each merged chunk's inherited section path unambiguous).
		for j < len(leaves) && leaves[j].ParentID == rep.ParentID && b.Len() < defaultChunkMergeTarget {
			b.WriteByte(10)
			b.WriteString(leaves[j].Text)
			j++
		}
		chunks = append(chunks, domain.Chunk{
			Body:     b.String(),
			Metadata: map[string]any{"struct_kind": rep.Kind, "section_path": rep.SectionPath},
		})
		reps = append(reps, rep)
		i = j
	}
	return chunks, reps
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
}

// ── persisted primitives ─────────────────────────────────────────────────────

// SectionRow is a structural section node persisted as a documents row.
type SectionRow struct {
	ID              string // "<documentID>::sec:<path>"
	DocumentID      string // the source document id
	Title           string
	Level           int
	Order           int
	SectionPath     string // human breadcrumb
	SectionLtree    string // ltree ordinal path (e.g. "n0.n2")
	ParentSectionID string // parent section row id, or "" if under the document root
}

// ChunkStamp inherits a leaf's structural location onto an ingested chunk.
type ChunkStamp struct {
	ChunkID         string
	SectionPath     string
	SectionLtree    string
	ParentSectionID string
}

// StructuralEdge is a typed structure-graph edge (PART_OF / NEXT).
type StructuralEdge struct {
	SourceID string
	TargetID string
	Type     domain.EdgeType
}

// StructureGraphStore persists the assembled structure graph. Implemented by the
// pgvector adapter; nil = structure graph disabled (legacy).
type StructureGraphStore interface {
	SaveSections(ctx context.Context, sections []SectionRow) error
	StampChunks(ctx context.Context, stamps []ChunkStamp) error
	SaveStructuralEdges(ctx context.Context, edges []StructuralEdge) error
}

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
