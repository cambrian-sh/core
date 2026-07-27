package postgres

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/memory"
)

// Document-structure graph persistence (ADR-0060). The adapter implements
// memory.StructureGraphStore: section nodes are stored as documents rows
// (document_type=doc_section, no embedding — excluded from fact recall), leaf
// chunks are stamped with their inherited section_path + ltree + parent section,
// and structural edges (PART_OF / NEXT) land in document_edges. All idempotent.

// StructureGraphStore returns the adapter as a memory.StructureGraphStore.
func (p *PgVectorAdapter) StructureGraphStore() memory.StructureGraphStore { return p }

// SaveSections upserts section nodes into the documents table.
func (p *PgVectorAdapter) SaveSections(ctx context.Context, sections []memory.SectionRow) error {
	for _, s := range sections {
		meta, _ := json.Marshal(map[string]any{
			"kind": "section", "level": s.Level, "order": s.Order, "title": s.Title,
			"document_id": s.DocumentID,
		})
		// ADR-0093: sections live in their own table. They carry no embedding and are
		// excluded from fact recall by design, so keeping them in the vector table made
		// every recall search traverse rows it could never return.
		//
		// document_id is written through a lookup rather than directly: a section whose
		// document row does not exist yet would fail the foreign key and abort an ingest
		// over bookkeeping, so an unresolvable parent stores NULL and the section is still
		// usable for structural navigation.
		_, err := p.pool.Exec(ctx, `
			INSERT INTO `+TableSections+`
				(id, document_id, title, metadata, section_path, parent_section_id, section_ltree)
			VALUES ($1, (SELECT id FROM `+TableDocuments+` WHERE id = $2), $3, $4, $5, $6, NULLIF($7,'')::ltree)
			ON CONFLICT (id) DO UPDATE SET
				document_id = EXCLUDED.document_id,
				title = EXCLUDED.title,
				metadata = EXCLUDED.metadata,
				section_path = EXCLUDED.section_path,
				parent_section_id = EXCLUDED.parent_section_id,
				section_ltree = EXCLUDED.section_ltree`,
			s.ID, s.DocumentID, s.Title, meta, s.SectionPath, s.ParentSectionID, s.SectionLtree)
		if err != nil {
			return mapError("SaveSections", err)
		}
	}
	return nil
}

// StampChunks writes the inherited structural location onto existing chunk rows.
func (p *PgVectorAdapter) StampChunks(ctx context.Context, stamps []memory.ChunkStamp) error {
	for _, s := range stamps {
		_, err := p.pool.Exec(ctx, `
			UPDATE `+TableChunks+`
			SET section_path = $2, parent_section_id = $3, section_ltree = NULLIF($4,'')::ltree
			WHERE id = $1`,
			s.ChunkID, s.SectionPath, s.ParentSectionID, s.SectionLtree)
		if err != nil {
			return mapError("StampChunks", err)
		}
	}
	return nil
}

// SaveStructuralEdges inserts PART_OF / NEXT edges. source_id FKs documents(id),
// so callers must persist section nodes (and chunks) before their edges.
func (p *PgVectorAdapter) SaveStructuralEdges(ctx context.Context, edges []memory.StructuralEdge) error {
	for _, e := range edges {
		_, err := p.pool.Exec(ctx, `
			INSERT INTO `+TableEdges+` (source_id, target_id, edge_type, weight)
			VALUES ($1, $2, $3, 1.0)
			ON CONFLICT (source_id, target_id, edge_type) DO NOTHING`,
			e.SourceID, e.TargetID, string(e.Type))
		if err != nil {
			return mapError("SaveStructuralEdges", err)
		}
	}
	return nil
}

// ChunksInMatchingSections powers section-scoped retrieval: it finds section
// nodes whose title (stored in text) or breadcrumb matches any query term, then
// returns the chunk ids in those sections' ltree subtrees. This is the
// structural analog of ChunksMentioningEntity — a locational query
// ("what does section 3.2 say") resolves to the chunks physically under 3.2,
// which pure vector similarity cannot localize.
func (p *PgVectorAdapter) ChunksInMatchingSections(ctx context.Context, terms []string, limit int) ([]string, error) {
	if len(terms) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	patterns := make([]string, 0, len(terms))
	for _, t := range terms {
		t = strings.TrimSpace(strings.ToLower(t))
		if len(t) >= 2 {
			patterns = append(patterns, "%"+t+"%")
		}
	}
	if len(patterns) == 0 {
		return nil, nil
	}
	// ADR-0093: what used to be a self-join filtered by document_type is now a join
	// between two tables that mean different things — sections and chunks. The shape of
	// the answer is unchanged; the query just stopped pretending they were the same kind
	// of row. `title` replaces `text` because that is what a section row now calls it.
	rows, err := p.pool.Query(ctx, `
		WITH matched AS (
			SELECT section_ltree FROM `+TableSections+`
			WHERE section_ltree IS NOT NULL
			  AND (lower(title) ILIKE ANY($1) OR lower(section_path) ILIKE ANY($1))
		)
		SELECT DISTINCT c.id
		FROM `+TableChunks+` c
		JOIN matched m ON c.section_ltree <@ m.section_ltree
		WHERE c.document_type = $2
		LIMIT $3`,
		patterns, domain.DocTypeMnemonicFact, limit)
	if err != nil {
		return nil, mapError("ChunksInMatchingSections", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, mapError("ChunksInMatchingSections scan", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
